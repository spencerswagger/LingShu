package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
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
}

func NewTOTPService(store *Store, sm4Key []byte) *TOTPService {
	return &TOTPService{store: store, sm4Key: sm4Key}
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
	secret, err := s.secret(ctx, userID)
	if err != nil {
		return nil, errBadRequest("请先完成 TOTP 初始化")
	}
	if !verifyCode(secret, code) {
		return nil, errBadRequest("验证码错误")
	}
	codes := make([]string, 0, recoveryNum)
	hashes := make([]string, 0, recoveryNum)
	for i := 0; i < recoveryNum; i++ {
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		plain := hex.EncodeToString(b)
		codes = append(codes, plain)
		hashes = append(hashes, tokenHash(plain))
	}
	if err := s.store.EnableTOTP(ctx, userID, hashes); err != nil {
		return nil, err
	}
	return codes, nil
}

// Verify 校验 TOTP code 或恢复码（恢复码一次性、用后作废）。返回是否有效。
func (s *TOTPService) Verify(ctx context.Context, userID int64, code string) (bool, error) {
	secret, err := s.secret(ctx, userID)
	if err == nil && secret != "" && verifyCode(secret, code) {
		return true, nil
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
func (s *TOTPService) Disable(ctx context.Context, userID int64, code string) error {
	ok, err := s.Verify(ctx, userID, code)
	if err != nil || !ok {
		return errBadRequest("验证码错误")
	}
	return s.store.DisableTOTP(ctx, userID)
}

// AdminForceDisable 管理员强制解绑（无需验证码）。
func (s *TOTPService) AdminForceDisable(ctx context.Context, userID int64) error {
	return s.store.DisableTOTP(ctx, userID)
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

func verifyCode(secret, code string) bool {
	ok, _ := totp.ValidateCustom(code, secret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return ok
}
