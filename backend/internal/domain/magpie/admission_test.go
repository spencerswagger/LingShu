package magpie

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
)

// ---- 准入层测试替身 ----

type fakeTokens struct {
	tok     *identity.Token
	err     error
	touched int64
}

func (f *fakeTokens) LookupByPlain(context.Context, string) (*identity.Token, error) {
	return f.tok, f.err
}

func (f *fakeTokens) TouchLastUsed(_ context.Context, id int64) error {
	f.touched = id
	return nil
}

type fakeModels struct {
	byName map[string]*model.ExternalModel
	list   []model.ExternalModel
	err    error
}

func (f *fakeModels) GetByExternalName(_ context.Context, name string) (*model.ExternalModel, error) {
	if f.err != nil {
		return nil, f.err
	}
	if m, ok := f.byName[name]; ok {
		return m, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeModels) ListModels(context.Context, *bool) ([]model.ExternalModel, error) {
	return f.list, f.err
}

type fakeKeys struct {
	key *Key
	err error
}

func (f *fakeKeys) ActiveByTokenID(context.Context, int64) (*Key, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.key, nil
}

func mp(s string) *string { return &s }

func activeToken() *identity.Token {
	return &identity.Token{ID: 7, UserID: 42, Status: identity.StatusTokenActive}
}

// newTestAdmitter 组装一个「全部放行」的准入层，测试按需覆盖单个替身。
func newTestAdmitter(m *fakeModels, k *fakeKeys, prefixes *PrefixProvider) *Admitter {
	if prefixes == nil {
		prefixes = NewPrefixProvider(nil)
	}
	return NewAdmitter(
		&fakeTokens{tok: activeToken()},
		m,
		k,
		[]byte("0123456789abcdef"),
		prefixes,
	)
}

// TestAdmit_Pass 断言三条齐备时放行并带出改写目标与凭据。
func TestAdmit_Pass(t *testing.T) {
	m := &fakeModels{byName: map[string]*model.ExternalModel{
		"tenant-gpt": {ID: 1, ExternalName: "tenant-gpt", Enabled: true, MagpieModel: mp("upstream/gpt-4o")},
	}}
	cipher, err := encryptForTest("caller-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	k := &fakeKeys{key: &Key{ID: 9, TokenID: 7, KeyName: "core-k1", KeyCipher: cipher, State: KeyStateActive}}

	dec, deny := newTestAdmitter(m, k, nil).Admit(context.Background(), "plain-token", "tenant-gpt")
	if deny != nil {
		t.Fatalf("应放行，实际拒绝: %+v", deny)
	}
	if dec.MagpieModel != "upstream/gpt-4o" || dec.CallerKey != "caller-secret" || dec.KeyName != "core-k1" {
		t.Fatalf("decision 不符: %+v", dec)
	}
}

// TestAdmit_Denies 覆盖所有拒绝分支的状态码与归类码（调用方按 code 分支）。
func TestAdmit_Denies(t *testing.T) {
	cipher, _ := encryptForTest("k")
	okModel := &model.ExternalModel{ID: 1, ExternalName: "m", Enabled: true, MagpieModel: mp("upstream/m")}
	disabled := &model.ExternalModel{ID: 2, ExternalName: "d", Enabled: false, MagpieModel: mp("upstream/d")}
	unmapped := &model.ExternalModel{ID: 3, ExternalName: "u", Enabled: true}
	subPool := &model.ExternalModel{ID: 4, ExternalName: "c", Enabled: true, MagpieModel: mp("claude/claude-sonnet-4")}

	cases := []struct {
		name      string
		tokens    *fakeTokens
		models    *fakeModels
		keys      *fakeKeys
		token     string
		modelName string
		wantStat  int
		wantCode  string
	}{
		{
			name: "缺少凭据", tokens: &fakeTokens{tok: activeToken()}, modelName: "m",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"m": okModel}},
			keys:     &fakeKeys{key: &Key{KeyCipher: cipher}},
			wantStat: 401, wantCode: CodeInvalidAPIKey,
		},
		{
			name: "令牌无效", tokens: &fakeTokens{err: errors.New("no")}, token: "t", modelName: "m",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"m": okModel}},
			keys:     &fakeKeys{key: &Key{KeyCipher: cipher}},
			wantStat: 401, wantCode: CodeInvalidAPIKey,
		},
		{
			name: "模型不存在", tokens: &fakeTokens{tok: activeToken()}, token: "t", modelName: "nope",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"m": okModel}},
			keys:     &fakeKeys{key: &Key{KeyCipher: cipher}},
			wantStat: 404, wantCode: CodeModelNotFound,
		},
		{
			name: "模型已停用（生产路径折叠成 ErrNotFound → 404）", tokens: &fakeTokens{tok: activeToken()}, token: "t", modelName: "d",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"d": disabled}},
			keys:     &fakeKeys{key: &Key{KeyCipher: cipher}},
			wantStat: 404, wantCode: CodeModelNotFound,
		},
		{
			name: "未映射代理模型", tokens: &fakeTokens{tok: activeToken()}, token: "t", modelName: "u",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"u": unmapped}},
			keys:     &fakeKeys{key: &Key{KeyCipher: cipher}},
			wantStat: 404, wantCode: CodeModelNotFound,
		},
		{
			name: "订阅池前缀拦截", tokens: &fakeTokens{tok: activeToken()}, token: "t", modelName: "c",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"c": subPool}},
			keys:     &fakeKeys{key: &Key{KeyCipher: cipher}},
			wantStat: 403, wantCode: CodeModelNotAllowed,
		},
		{
			name: "令牌未开通代理访问", tokens: &fakeTokens{tok: activeToken()}, token: "t", modelName: "m",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"m": okModel}},
			keys:     &fakeKeys{err: ErrKeyNotProvisioned},
			wantStat: 403, wantCode: CodeMagpieKeyMissing,
		},
		{
			name: "凭据密文不可用", tokens: &fakeTokens{tok: activeToken()}, token: "t", modelName: "m",
			models:   &fakeModels{byName: map[string]*model.ExternalModel{"m": okModel}},
			keys:     &fakeKeys{key: &Key{KeyCipher: ""}},
			wantStat: 403, wantCode: CodeMagpieKeyMissing,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewAdmitter(c.tokens, c.models, c.keys, []byte("0123456789abcdef"), NewPrefixProvider(nil))
			_, deny := a.Admit(context.Background(), c.token, c.modelName)
			if deny == nil {
				t.Fatalf("期望拒绝")
			}
			if deny.Status != c.wantStat || deny.Code != c.wantCode {
				t.Fatalf("deny = %+v, 期望 %d/%s", deny, c.wantStat, c.wantCode)
			}
		})
	}
}

// TestPrefixProvider_SourcesAndFallback 断言前缀名单来源与失败方向（失败仍拦截）。
func TestPrefixProvider_SourcesAndFallback(t *testing.T) {
	t.Run("读取配置覆盖默认名单", func(t *testing.T) {
		p := NewPrefixProvider(func(context.Context, string) ([]byte, error) {
			return []byte(`["  Custom/ ", "",""]`), nil
		})
		got := p.Prefixes(context.Background())
		if len(got) != 1 || got[0] != "custom/" {
			t.Fatalf("prefixes = %v, 期望 [custom/]", got)
		}
	})
	t.Run("配置写入后失效即时生效", func(t *testing.T) {
		var raw = []byte(`["a/"]`)
		p := NewPrefixProvider(func(context.Context, string) ([]byte, error) { return raw, nil })
		if got := p.Prefixes(context.Background()); len(got) != 1 || got[0] != "a/" {
			t.Fatalf("首次读取 = %v", got)
		}
		raw = []byte(`["b/"]`)
		if got := p.Prefixes(context.Background()); got[0] != "a/" {
			t.Fatalf("缓存应命中，实际 %v", got)
		}
		p.Invalidate()
		if got := p.Prefixes(context.Background()); got[0] != "b/" {
			t.Fatalf("失效后应重新读取，实际 %v", got)
		}
	})
	t.Run("读失败回落默认名单（继续拦截）", func(t *testing.T) {
		p := NewPrefixProvider(func(context.Context, string) ([]byte, error) {
			return nil, errors.New("db down")
		})
		got := p.Prefixes(context.Background())
		if len(got) != len(DefaultSubscriptionPrefixes) {
			t.Fatalf("应回落默认名单，实际 %v", got)
		}
		if !isSubscriptionModel("claude/claude-sonnet-4", got) {
			t.Fatal("读失败时 claude/ 前缀仍必须被拦截")
		}
	})
	t.Run("空数组回落默认名单", func(t *testing.T) {
		p := NewPrefixProvider(func(context.Context, string) ([]byte, error) { return []byte(`[]`), nil })
		if got := p.Prefixes(context.Background()); len(got) != len(DefaultSubscriptionPrefixes) {
			t.Fatalf("应回落默认名单，实际 %v", got)
		}
	})
}

// TestListTenantModels_FiltersLikeAdmission 断言 /v1/models 与准入同一口径（列出即调得通）。
func TestListTenantModels_FiltersLikeAdmission(t *testing.T) {
	m := &fakeModels{list: []model.ExternalModel{
		{ExternalName: "ok", Enabled: true, MagpieModel: mp("upstream/ok")},
		{ExternalName: "unmapped", Enabled: true},
		{ExternalName: "sub", Enabled: true, MagpieModel: mp("codex/gpt-5")},
		{ExternalName: "ok2", Enabled: true, MagpieModel: mp("Gemini-2.0-Flash")},
	}}
	a := newTestAdmitter(m, &fakeKeys{}, nil)
	got, err := a.ListTenantModels(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0] != "ok" || got[1] != "ok2" {
		t.Fatalf("可见模型 = %v, 期望 [ok ok2]", got)
	}
}

// TestExtractPlainToken 断言凭据提取：Bearer 优先，Anthropic 客户端兼容 x-api-key。
func TestExtractPlainToken(t *testing.T) {
	mk := func(auth, apiKey string) map[string][]string {
		h := map[string][]string{}
		if auth != "" {
			h["Authorization"] = []string{auth}
		}
		if apiKey != "" {
			h["X-Api-Key"] = []string{apiKey}
		}
		return h
	}
	cases := []struct {
		name    string
		headers map[string][]string
		want    string
	}{
		{name: "Bearer", headers: mk("Bearer abc", ""), want: "abc"},
		{name: "Bearer 前置空白", headers: mk("Bearer   abc  ", ""), want: "abc"},
		{name: "仅 x-api-key", headers: mk("", "sk-ant"), want: "sk-ant"},
		{name: "Bearer 优先", headers: mk("Bearer b1", "sk-ant"), want: "b1"},
		{name: "非 Bearer 且无 x-api-key", headers: mk("Basic xyz", ""), want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := httpHeaderOf(c.headers)
			if got := ExtractPlainToken(h); got != c.want {
				t.Fatalf("got %q, 期望 %q", got, c.want)
			}
		})
	}
}
