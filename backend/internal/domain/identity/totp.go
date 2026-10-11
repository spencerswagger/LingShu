package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/reqmeta"
)

const (
	totpIssuer  = "llmgateway"
	preAuthTTL  = 5 * time.Minute
	recoveryNum = 10
	// maxPreAuthEntries PreAuthStore 内存条数上限：达上限时淘汰最旧条目（而非拒绝签发），防无限增长。
	maxPreAuthEntries = 10000
)

// PreAuthStore 进程内一次性登录预授权（2FA 第一步产物）。
type PreAuthStore struct {
	mu chan struct{}
	m  map[string]preAuthEntry
	// byUser 记录每用户当前有效的 token，实现「同一用户仅保留最新一个」。
	byUser map[int64]string
	now    func() time.Time
}

type preAuthEntry struct {
	userID int64
	exp    time.Time
}

func NewPreAuthStore() *PreAuthStore {
	return &PreAuthStore{
		mu:     make(chan struct{}, 1),
		m:      map[string]preAuthEntry{},
		byUser: map[int64]string{},
		now:    time.Now,
	}
}

// Issue 签发一次性 preauth token（5 分钟有效）。
// 同一用户仅保留最新一个 token（签发即作废旧 token）；存储达上限时淘汰最旧条目
// 而非拒绝签发——避免攻击者灌满存储导致全体 2FA 用户第二步登录失败（DoS）。
func (p *PreAuthStore) Issue(userID int64) (string, bool) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", false
	}
	tok := hex.EncodeToString(b)
	p.mu <- struct{}{}
	defer func() { <-p.mu }()
	now := p.now()
	p.pruneLocked(now)
	if old, ok := p.byUser[userID]; ok {
		p.removeLocked(old)
	}
	if len(p.m) >= maxPreAuthEntries {
		p.evictOldestLocked()
	}
	p.m[tok] = preAuthEntry{userID: userID, exp: now.Add(preAuthTTL)}
	p.byUser[userID] = tok
	return tok, true
}

// Peek 只读校验 preauth token：命中且未过期返回 userID，但不删除。
// 供「第二步限流 + 校验通过后再消费」的顺序使用：失败/限流时不销毁会话，允许重试。
func (p *PreAuthStore) Peek(token string) (int64, bool) {
	p.mu <- struct{}{}
	defer func() { <-p.mu }()
	now := p.now()
	p.pruneLocked(now)
	e, ok := p.m[token]
	if !ok || now.After(e.exp) {
		return 0, false
	}
	return e.userID, true
}

// Consume 消费一次性 preauth token，成功返回 userID 并删除。
func (p *PreAuthStore) Consume(token string) (int64, bool) {
	p.mu <- struct{}{}
	defer func() { <-p.mu }()
	now := p.now()
	p.pruneLocked(now)
	e, ok := p.m[token]
	if !ok {
		return 0, false
	}
	p.removeLocked(token)
	if now.After(e.exp) {
		return 0, false
	}
	return e.userID, true
}

// pruneLocked 惰性清理已过期条目，防止 TTL 只在校验时判定导致的内存无限增长。
// 必须在持锁下调用。
func (p *PreAuthStore) pruneLocked(now time.Time) {
	for k, e := range p.m {
		if now.After(e.exp) {
			p.removeLocked(k)
		}
	}
}

// evictOldestLocked 淘汰最早过期（即最早签发）的一条，为新签发腾位。必须在持锁下调用。
func (p *PreAuthStore) evictOldestLocked() {
	oldestKey := ""
	var oldestExp time.Time
	for k, e := range p.m {
		if oldestKey == "" || e.exp.Before(oldestExp) {
			oldestKey, oldestExp = k, e.exp
		}
	}
	if oldestKey != "" {
		p.removeLocked(oldestKey)
	}
}

// removeLocked 删除一条 token 并同步清理 byUser 反查。必须在持锁下调用。
func (p *PreAuthStore) removeLocked(token string) {
	e, ok := p.m[token]
	if !ok {
		return
	}
	delete(p.m, token)
	if p.byUser[e.userID] == token {
		delete(p.byUser, e.userID)
	}
}

// TOTPService 负责 TOTP 绑定/确认/解绑/校验与恢复码。
type TOTPService struct {
	store  *Store
	sm4Key []byte
	audit  AuditSink
}

func NewTOTPService(store *Store, sm4Key []byte) *TOTPService {
	return &TOTPService{store: store, sm4Key: sm4Key}
}

// SetAudit 注入审计存储（nil 时跳过审计，不阻塞业务）。
func (s *TOTPService) SetAudit(a AuditSink) { s.audit = a }

func (s *TOTPService) auditLog(ctx context.Context, e audit.Entry) {
	if m := reqmeta.From(ctx); m.IP != "" || m.RequestID != "" {
		if e.IP == "" {
			e.IP = m.IP
		}
		if e.RequestID == "" {
			e.RequestID = m.RequestID
		}
	}
	if s.audit == nil {
		return
	}
	if err := s.audit.Insert(ctx, e); err != nil {
		slog.ErrorContext(ctx, "audit insert failed", "action", e.Action, "err", err)
	}
}

// Setup 生成新 secret（覆盖未确认的旧 pending），返回 otpauth URI 与 base32 secret。
// 已启用 2FA 时拒绝：Setup 会把 totp_enabled 置回 false，若允许直接调用，
// 就等于提供了一条「免动态码即可静默关闭 2FA」的捷径，绕过 Disable 的验码保护。
// 重绑需先经 Disable（要求动态码）关闭后再重新绑定。
func (s *TOTPService) Setup(ctx context.Context, userID int64, username string) (uri, secret string, err error) {
	cipher, enabled, err := s.store.TOTPSecret(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", errBadRequest("用户不存在")
		}
		return "", "", err
	}
	if enabled && cipher != "" {
		return "", "", errBadRequest("已开启两步验证，请先关闭后再重新绑定")
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: username,
		Period:      30,
		SecretSize:  20,
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", err
	}
	cipher, err = crypto.SM4Encrypt(s.sm4Key, []byte(key.Secret()))
	if err != nil {
		return "", "", err
	}
	if err := s.store.SetTOTPSecret(ctx, userID, cipher); err != nil {
		return "", "", err
	}
	return key.URL(), key.Secret(), nil
}

// Confirm 确认绑定：校验 code 后启用并生成恢复码（SM3 哈希落库），返回明文恢复码一次。
func (s *TOTPService) Confirm(ctx context.Context, userID int64, code string) ([]string, error) {
	secret, err := s.pendingSecret(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !verifyCode(secret, code) {
		return nil, errBadRequest("验证码错误")
	}
	codes := make([]string, 0, recoveryNum)
	hashes := make([]string, 0, recoveryNum)
	for i := 0; i < recoveryNum; i++ {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		plain := hex.EncodeToString(b)
		codes = append(codes, plain)
		hashes = append(hashes, tokenHash(plain))
	}
	if err := s.store.EnableTOTP(ctx, userID, hashes); err != nil {
		return nil, err
	}
	s.auditLog(ctx, audit.Entry{UserID: userID, Action: "totp.enable", TargetType: "user", TargetID: strconv.FormatInt(userID, 10)})
	return codes, nil
}

// Verify 校验 TOTP code 或恢复码。动态码只要在有效时间窗内即通过，可重复使用——
// 已移除「同一时间步防重放」：单一 last_step 记录会让反复登录的用户把合法的最新码
// 误判为已消费而被拒（见线上反馈：开启 2FA 后反复登录频繁报错）。恢复码仍一次性。
func (s *TOTPService) Verify(ctx context.Context, userID int64, code string) (bool, error) {
	secret, err := s.secret(ctx, userID)
	if err == nil && secret != "" {
		if verifyCode(secret, code) {
			return true, nil
		}
	}
	// 恢复码分支
	hashes, herr := s.store.GetRecoveryHashes(ctx, userID)
	if herr != nil || len(hashes) == 0 {
		return false, nil
	}
	codeHash := tokenHash(code)
	for i, h := range hashes {
		if h == codeHash {
			hashes = append(hashes[:i], hashes[i+1:]...)
			if err := s.store.SetRecoveryHashes(ctx, userID, hashes); err != nil {
				// 消费失败则该恢复码未被作废（防重放 fail-open）：必须留痕告警，
				// 否则同一恢复码可被重复使用且无人察觉。
				slog.ErrorContext(ctx, "consume recovery code failed, replay protection degraded",
					"user_id", userID, "err", err)
				s.auditLog(ctx, audit.Entry{UserID: userID, Action: "totp.recovery.consume_fail",
					TargetType: "user", TargetID: strconv.FormatInt(userID, 10)})
			}
			return true, nil
		}
	}
	return false, nil
}

// Disable 解绑：校验 code（或恢复码）后清空 TOTP 与恢复码。
func (s *TOTPService) Disable(ctx context.Context, userID int64, code string) error {
	ok, err := s.Verify(ctx, userID, code)
	if err != nil || !ok {
		return errBadRequest("验证码错误")
	}
	if err := s.store.DisableTOTP(ctx, userID); err != nil {
		return err
	}
	s.auditLog(ctx, audit.Entry{UserID: userID, Action: "totp.disable", TargetType: "user", TargetID: strconv.FormatInt(userID, 10)})
	return nil
}

// AdminForceDisable 管理员强制解绑（无需验证码）。
func (s *TOTPService) AdminForceDisable(ctx context.Context, userID int64) error {
	if err := s.store.DisableTOTP(ctx, userID); err != nil {
		return err
	}
	s.auditLog(ctx, audit.Entry{Action: "totp.admin_force_disable", TargetType: "user", TargetID: strconv.FormatInt(userID, 10)})
	return nil
}

func (s *TOTPService) secret(ctx context.Context, userID int64) (string, error) {
	cipher, enabled, err := s.store.TOTPSecret(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errBadRequest("用户不存在")
		}
		return "", err
	}
	if !enabled || cipher == "" {
		return "", errBadRequest("尚未启用 TOTP")
	}
	b, err := crypto.SM4Decrypt(s.sm4Key, cipher)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// pendingSecret 取"待确认绑定"的 TOTP secret（不要求已启用），供 Confirm 使用。
func (s *TOTPService) pendingSecret(ctx context.Context, userID int64) (string, error) {
	cipher, _, err := s.store.TOTPSecret(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errBadRequest("用户不存在")
		}
		return "", err
	}
	if cipher == "" {
		return "", errBadRequest("请先完成 TOTP 初始化")
	}
	b, err := crypto.SM4Decrypt(s.sm4Key, cipher)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// verifyCode 校验动态码是否命中当前 ±1 步（共 3 个 30s 窗口）中的任一时间步。
func verifyCode(secret, code string) bool {
	base := time.Now().Unix() / 30
	for _, off := range []int64{0, -1, 1} {
		ok, _ := totp.ValidateCustom(code, secret, time.Unix((base+off)*30, 0), totp.ValidateOpts{
			Period:    30,
			Skew:      0,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if ok {
			return true
		}
	}
	return false
}
