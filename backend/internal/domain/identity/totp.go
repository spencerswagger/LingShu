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
)

const (
	totpIssuer  = "llmgateway"
	preAuthTTL  = 5 * time.Minute
	recoveryNum = 10
)

// PreAuthStore 进程内一次性登录预授权（2FA 第一步产物）。
type PreAuthStore struct {
	mu  chan struct{}
	m   map[string]preAuthEntry
	now func() time.Time
}

type preAuthEntry struct {
	userID int64
	exp    time.Time
}

func NewPreAuthStore() *PreAuthStore {
	return &PreAuthStore{mu: make(chan struct{}, 1), m: map[string]preAuthEntry{}, now: time.Now}
}

// Issue 签发一次性 preauth token（5 分钟有效）。
func (p *PreAuthStore) Issue(userID int64) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	p.mu <- struct{}{}
	p.m[tok] = preAuthEntry{userID: userID, exp: p.now().Add(preAuthTTL)}
	<-p.mu
	return tok
}

// Consume 消费一次性 preauth token，成功返回 userID 并删除。
func (p *PreAuthStore) Consume(token string) (int64, bool) {
	p.mu <- struct{}{}
	defer func() { <-p.mu }()
	e, ok := p.m[token]
	if !ok {
		return 0, false
	}
	delete(p.m, token)
	if p.now().After(e.exp) {
		return 0, false
	}
	return e.userID, true
}

// TOTPService 负责 TOTP 绑定/确认/解绑/校验与恢复码。
type TOTPService struct {
	store  *Store
	sm4Key []byte
	audit  *audit.Store
}

func NewTOTPService(store *Store, sm4Key []byte) *TOTPService {
	return &TOTPService{store: store, sm4Key: sm4Key}
}

// SetAudit 注入审计存储（nil 时跳过审计，不阻塞业务）。
func (s *TOTPService) SetAudit(a *audit.Store) { s.audit = a }

func (s *TOTPService) auditLog(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Insert(ctx, e); err != nil {
		slog.ErrorContext(ctx, "audit insert failed", "action", e.Action, "err", err)
	}
}

// Setup 生成新 secret（覆盖未确认的旧 pending），返回 otpauth URI 与 base32 secret。
func (s *TOTPService) Setup(ctx context.Context, userID int64, username string) (uri, secret string, err error) {
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
	cipher, err := crypto.SM4Encrypt(s.sm4Key, []byte(key.Secret()))
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
	if ok, _ := verifyCode(secret, code); !ok {
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

// Verify 校验 TOTP code 或恢复码（均一次性：动态码按时间步防重放，恢复码用后作废）。
func (s *TOTPService) Verify(ctx context.Context, userID int64, code string) (bool, error) {
	return s.verify(ctx, userID, code, true)
}

// verify 校验 code（或恢复码）。consume=true 时对动态码做防重放消费（记录命中时间步），
// consume=false 仅验码有效性（解绑场景已要求认证会话，重放不带来额外权限）。
func (s *TOTPService) verify(ctx context.Context, userID int64, code string, consume bool) (bool, error) {
	secret, err := s.secret(ctx, userID)
	if err == nil && secret != "" {
		if ok, step := verifyCode(secret, code); ok {
			if !consume {
				return true, nil
			}
			last, lerr := s.store.TOTPLastStep(ctx, userID)
			if lerr != nil {
				return false, nil
			}
			if step > last {
				if serr := s.store.SetTOTPLastStep(ctx, userID, step); serr != nil {
					// 写失败则该步未被记录（防重放 fail-open）：不拒绝登录（避免 DB 抖动升级为全员无法登录），
					// 但必须留痕告警，便于运维发现防重放保护正在降级。
					slog.WarnContext(ctx, "record totp last step failed, replay protection degraded",
						"user_id", userID, "step", step, "err", serr)
				}
				return true, nil
			}
			return false, nil // 该时间步已消费过 → 重放
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
			_ = s.store.SetRecoveryHashes(ctx, userID, hashes)
			return true, nil
		}
	}
	return false, nil
}

// Disable 解绑：校验 code（或恢复码）后清空 TOTP 与恢复码。
// 不消费时间步——解绑已要求已认证会话，且同一 30s 窗口内刚用于登录的那个码
// 若被判重放，会让"登录后顺手关闭 2FA"这一常见操作莫名失败。
func (s *TOTPService) Disable(ctx context.Context, userID int64, code string) error {
	ok, err := s.verify(ctx, userID, code, false)
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

// verifyCode 校验动态码，返回是否命中与命中的时间步（未命中返回 false, 0）。
// 时间窗为当前 ±1 步（共 3 个 30s 窗口），按步回推精确判定。
//
// 已知取舍：客户端时钟偏差约 ±30s 时，设备可能连续生成"上一步"的码，而防重放
// 已把该步记为已消费 → 短时间内重复做 2FA 会被短暂误拒（窗口最长为漂移时长，≤30s）。
// 现实中极少触发（通常已有会话）；若需精确，可改为记录已消费步集合而非单一 last。
func verifyCode(secret, code string) (bool, int64) {
	base := time.Now().Unix() / 30
	for _, off := range []int64{0, -1, 1} {
		step := base + off
		ok, _ := totp.ValidateCustom(code, secret, time.Unix(step*30, 0), totp.ValidateOpts{
			Period:    30,
			Skew:      0,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if ok {
			return true, step
		}
	}
	return false, 0
}
