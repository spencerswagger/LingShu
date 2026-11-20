// callersync_test.go 锁改名回填的幂等三步：
//
//	① magpie 侧 rename-key 原地改（值不变）→ ② 确保新名明文 → ③ 本地行改名+激活+留旧名。
//
// 幂等要求：重复执行不产生重复数据、不破坏既有状态、中断后重跑能续上。
package magpie

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// fakeKeyStore 记录全部写入动作（renames / provisions / disables），可注入故障。
type fakeKeyStore struct {
	mu         sync.Mutex
	byToken    map[int64]*Key
	renames    [][2]string // (old,new) 序列
	provision  int
	failRename bool
}

func newFakeKeyStore() *fakeKeyStore { return &fakeKeyStore{byToken: map[int64]*Key{}} }

func (f *fakeKeyStore) ByTokenID(_ context.Context, tokenID int64) (*Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.byToken[tokenID]
	if !ok {
		return nil, ErrKeyNotProvisioned
	}
	cp := *k
	return &cp, nil
}

func (f *fakeKeyStore) Provision(_ context.Context, tokenID int64, keyName, keyCipher string) (*Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provision++
	f.byToken[tokenID] = &Key{ID: 1, TokenID: tokenID, KeyName: keyName, KeyCipher: keyCipher, State: KeyStateActive}
	k := *f.byToken[tokenID]
	return &k, nil
}

func (f *fakeKeyStore) Disable(_ context.Context, tokenID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.byToken[tokenID]
	if !ok {
		return ErrKeyNotProvisioned
	}
	k.State = KeyStateDisabled
	return nil
}

func (f *fakeKeyStore) Rename(_ context.Context, tokenID int64, oldName, newName string) (*Key, error) {
	return f.RenameAndActivate(context.Background(), tokenID, oldName, newName, "")
}

func (f *fakeKeyStore) RenameAndActivate(_ context.Context, tokenID int64, oldName, newName, keyCipher string) (*Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRename {
		return nil, fmt.Errorf("injected rename failure")
	}
	f.renames = append(f.renames, [2]string{oldName, newName})
	k, ok := f.byToken[tokenID]
	if !ok {
		return nil, ErrKeyNotProvisioned
	}
	k.KeyName = newName
	k.LegacyKeyName = oldName
	if keyCipher != "" {
		k.KeyCipher = keyCipher
	}
	k.State = KeyStateActive
	cp := *k
	return &cp, nil
}

func newSyncForTest(t *testing.T, pageSrv string, keys CallerKeyStore) *CallerKeySync {
	t.Helper()
	return NewCallerKeySync(NewPageAdmin(pageSrv, syncTestPageKey, nil), keys, []byte("0123456789abcdef"))
}

const syncTestPageKey = "aaaabbbbccccddddeeeeffff00001111"

// TestEnsure_RenameLegacyNameToNewFormat 改名主链：旧格式名 → 新格式名，值不变、旧名留底。
func TestEnsure_RenameLegacyNameToNewFormat(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	// 预置：令牌 7（用户 12）已有旧格式映射。
	fk.Provision(context.Background(), 7, "ls-token-7", "old-cipher")
	if _, ok := pageKeys.Load("ls-token-7"); !ok {
		// 代理侧也预置同名 key（真机先有旧名）。
		if _, err := NewPageAdmin(srv.URL, syncTestPageKey, nil).EnsureCallerKey(context.Background(), "ls-token-7", "secret-xyz"); err != nil {
			t.Fatalf("preset: %v", err)
		}
	}
	s := newSyncForTest(t, srv.URL, fk)

	err := s.Ensure(context.Background(), 12, TokenRef{ID: 7, DisplayName: "Claude Code"}, "secret-xyz")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := KeyNameFor(12, 7, "Claude Code")
	if want != "ls-u12-t7-claude-code" {
		t.Fatalf("目标名不符：%q", want)
	}
	// 代理侧：旧名没了、新名值不变。
	if _, ok := pageKeys.Load("ls-token-7"); ok {
		t.Fatal("代理侧旧名应已改名消失")
	}
	if v, ok := pageKeys.Load(want); !ok || v != "secret-xyz" {
		t.Fatalf("代理侧新名/值不符：ok=%v v=%v", ok, v)
	}
	// 本地：名字换新、旧名留底、state ACTIVE。
	k, _ := fk.ByTokenID(context.Background(), 7)
	if k.KeyName != want || k.LegacyKeyName != "ls-token-7" || k.State != KeyStateActive {
		t.Fatalf("本地行不符：%+v", k)
	}
	// 明文值不变：密文能解出原 secret（fake 里保存的是传入密文；这里比对改名未换值）。
	if len(fk.renames) != 1 || fk.renames[0] != [2]string{"ls-token-7", want} {
		t.Fatalf("rename 序列不符：%v", fk.renames)
	}
}

// TestEnsure_IdempotentRerun 重复执行收敛：第二次 Ensure 全部命中「已是终态」，
// rename 序列不再增长（不重复改名、不产生重复数据）。
func TestEnsure_IdempotentRerun(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "ls-token-7", "old-cipher")
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()
	arg := TokenRef{ID: 7, DisplayName: "bot"}

	if err := s.Ensure(ctx, 12, arg, "secret-xyz"); err != nil {
		t.Fatalf("first: %v", err)
	}
	n1 := len(fk.renames)
	p1 := fk.provision
	pageN1 := mapCount(pageKeys)
	for i := 0; i < 3; i++ {
		if err := s.Ensure(ctx, 12, arg, "secret-xyz"); err != nil {
			t.Fatalf("rerun %d: %v", i, err)
		}
	}
	if len(fk.renames) != n1 {
		t.Fatalf("重跑产生了新的 rename：%v", fk.renames)
	}
	if fk.provision != p1 {
		t.Fatalf("重跑不应新增 Provision（终态已达成）：%d → %d", p1, fk.provision)
	}
	if mapCount(pageKeys) != pageN1 {
		t.Fatalf("重跑改变了代理侧 key 数量（产生重复数据）：%d → %d", pageN1, mapCount(pageKeys))
	}
	want := KeyNameFor(12, 7, "bot")
	if v, ok := pageKeys.Load(want); !ok || v != "secret-xyz" {
		t.Fatalf("终态不符：ok=%v v=%v", ok, v)
	}
}

// mapCount 统计 fake 代理侧 key 总数（断言「不产生重复数据」）。
func mapCount(m *sync.Map) int {
	n := 0
	m.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestEnsure_InterruptThenResume 中断续跑：改完 magpie 侧、本地行改名失败 →
// 重跑时本地行还是旧名，Ensure 从实际状态继续推进（magpie 侧幂等吞掉重复 rename）。
func TestEnsure_InterruptThenResume(t *testing.T) {
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "ls-token-7", "old-cipher")
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()

	// 注入本地改名失败：magpie 侧已改名、本地行未动。
	fk.failRename = true
	if err := s.Ensure(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "secret-xyz"); err == nil {
		t.Fatal("注入失败应返回错误")
	}
	fk.failRename = false
	k, _ := fk.ByTokenID(ctx, 7)
	if k.KeyName != "ls-token-7" {
		t.Fatalf("本地行不应被改：%+v", k)
	}
	// 重跑：从实际状态续上，终态一致。
	if err := s.Ensure(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "secret-xyz"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	k, _ = fk.ByTokenID(ctx, 7)
	if k.KeyName != KeyNameFor(12, 7, "bot") || k.LegacyKeyName != "ls-token-7" || k.State != KeyStateActive {
		t.Fatalf("续跑终态不符：%+v", k)
	}
}

// TestEnsure_ManualNamedUntouched 手工绑定名不被抢：非 ls-token-<id> 名字保持原样，
// 只对齐代理侧明文与本地 state。
func TestEnsure_ManualNamedUntouched(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 9, "ops-manual-key", "old-cipher")
	s := newSyncForTest(t, srv.URL, fk)

	if err := s.Ensure(context.Background(), 3, TokenRef{ID: 9, DisplayName: "whatever"}, "secret-9"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	k, _ := fk.ByTokenID(context.Background(), 9)
	if k.KeyName != "ops-manual-key" || k.LegacyKeyName != "" {
		t.Fatalf("手工名被动了：%+v", k)
	}
	if v, ok := pageKeys.Load("ops-manual-key"); !ok || v != "secret-9" {
		t.Fatalf("代理侧值未对齐：ok=%v v=%v", ok, v)
	}
}

// TestEnsure_NewTokenNoBinding 无映射 → 按新名建（建令牌路径）。
func TestEnsure_NewTokenNoBinding(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	s := newSyncForTest(t, srv.URL, fk)

	if err := s.Ensure(context.Background(), 12, TokenRef{ID: 31, DisplayName: "claude-code"}, "secret-31"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := KeyNameFor(12, 31, "claude-code")
	if v, ok := pageKeys.Load(want); !ok || v != "secret-31" {
		t.Fatalf("代理侧新建不符：ok=%v v=%v", ok, v)
	}
	k, _ := fk.ByTokenID(context.Background(), 31)
	if k == nil || k.KeyName != want || k.LegacyKeyName != "" {
		t.Fatalf("本地新建不符：%+v", k)
	}
}
