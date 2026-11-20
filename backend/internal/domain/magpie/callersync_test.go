// callersync_test.go 锁改名回填/消歧/回滚的全部幂等行为：
//
//	改名三步：① magpie 侧 rename-key 原地改（值不变）→ ② 确保新名明文 → ③ 本地行改名+激活+留旧名。
//	消歧：基础名撞 → -2/-3 顺序找空位；重试一轮仍撞 → ErrNameUnavailable（409 挂起人工）。
//	回滚：按 legacy_key_name 复原旧名，三步与正向镜像、可幂等重跑。
//
// 幂等要求：重复执行不产生重复数据、不破坏既有状态、中断后重跑能续上。
package magpie

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeKeyStore 记录全部写入动作（renames / provisions / disables），可注入故障。
type fakeKeyStore struct {
	mu          sync.Mutex
	byToken     map[int64]*Key
	renames     [][2]string // (old,new) 序列
	provision   int
	failRename  bool
	failClear   bool
	occupied    map[string]bool // 测试注入的「名字已被其它令牌占用」集合
	clearCalls  int
	legacyClear []int64
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{byToken: map[int64]*Key{}, occupied: map[string]bool{}}
}

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

func (f *fakeKeyStore) NameOccupied(_ context.Context, keyName string, excludeTokenID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.occupied[keyName] {
		// 注入的占用若恰好属于本令牌则不算（测试里不构造这种形状）。
		return true, nil
	}
	for tid, k := range f.byToken {
		if tid == excludeTokenID {
			continue
		}
		if k.KeyName == keyName || (k.LegacyKeyName == keyName && k.LegacyKeyName != "") {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeKeyStore) RenameRollbackTargets(_ context.Context, limit int) ([]Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Key
	for _, k := range f.byToken {
		if k.State == KeyStateActive && k.LegacyKeyName != "" {
			out = append(out, *k)
		}
	}
	return out, nil
}

func (f *fakeKeyStore) ClearLegacyName(_ context.Context, tokenID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failClear {
		return fmt.Errorf("injected clear failure")
	}
	f.clearCalls++
	f.legacyClear = append(f.legacyClear, tokenID)
	k, ok := f.byToken[tokenID]
	if !ok {
		return ErrKeyNotProvisioned
	}
	k.LegacyKeyName = ""
	return nil
}

func newSyncForTest(t *testing.T, pageSrv string, keys CallerKeyStore) *CallerKeySync {
	t.Helper()
	return NewCallerKeySync(NewPageAdmin(pageSrv, syncTestPageKey, nil), keys, []byte("0123456789abcdef"))
}

const syncTestPageKey = "aaaabbbbccccddddeeeeffff00001111"

// TestEnsure_RenameLegacyNameToNewFormat 改名主链：旧格式名 → 新契约名，值不变、旧名留底。
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

	err := s.Ensure(context.Background(), 12, TokenRef{ID: 7, DisplayName: "办公室台式机"}, "zhangsan", "secret-xyz")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := "zhangsan-w-5866" // 令牌名全中文 → 段退化 t<tokenID>
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

	if err := s.Ensure(ctx, 12, arg, "zhangsan", "secret-xyz"); err != nil {
		t.Fatalf("first: %v", err)
	}
	n1 := len(fk.renames)
	p1 := fk.provision
	pageN1 := mapCount(pageKeys)
	for i := 0; i < 3; i++ {
		if err := s.Ensure(ctx, 12, arg, "zhangsan", "secret-xyz"); err != nil {
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
	want := "zhangsan-bot"
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
	if err := s.Ensure(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); err == nil {
		t.Fatal("注入失败应返回错误")
	}
	fk.failRename = false
	k, _ := fk.ByTokenID(ctx, 7)
	if k.KeyName != "ls-token-7" {
		t.Fatalf("本地行不应被改：%+v", k)
	}
	// 重跑：从实际状态续上，终态一致。
	if err := s.Ensure(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	k, _ = fk.ByTokenID(ctx, 7)
	if k.KeyName != "zhangsan-bot" || k.LegacyKeyName != "ls-token-7" || k.State != KeyStateActive {
		t.Fatalf("续跑终态不符：%+v", k)
	}
}

// TestEnsure_ManualNamedUntouched 手工绑定名不被抢：非旧自动名保持原样，
// 只对齐代理侧明文与本地 state。
func TestEnsure_ManualNamedUntouched(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 9, "ops-manual-key", "old-cipher")
	s := newSyncForTest(t, srv.URL, fk)

	if err := s.Ensure(context.Background(), 3, TokenRef{ID: 9, DisplayName: "whatever"}, "lisi", "secret-9"); err != nil {
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

// TestEnsure_NewTokenNoBinding 无映射 → 按契约名建（建令牌路径）。
func TestEnsure_NewTokenNoBinding(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	s := newSyncForTest(t, srv.URL, fk)

	if err := s.Ensure(context.Background(), 12, TokenRef{ID: 31, DisplayName: "claude-code"}, "zhangsan", "secret-31"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := "zhangsan-claude-code"
	if v, ok := pageKeys.Load(want); !ok || v != "secret-31" {
		t.Fatalf("代理侧新建不符：ok=%v v=%v", ok, v)
	}
	k, _ := fk.ByTokenID(context.Background(), 31)
	if k == nil || k.KeyName != want || k.LegacyKeyName != "" {
		t.Fatalf("本地新建不符：%+v", k)
	}
}

// TestEnsureNameUnique_Disambiguation 消歧：基础名被其它令牌占用 → -2 顺延找空位。
func TestEnsureNameUnique_Disambiguation(t *testing.T) {
	ctx := context.Background()
	s := newSyncForTest(t, mustFakePage(t), newFakeKeyStore())
	fk := s.keys.(*fakeKeyStore)
	// 令牌 2 先占住 zhangsan-bot。
	fk.Provision(ctx, 2, "zhangsan-bot", "cipher-2")
	got, err := s.EnsureNameUnique(ctx, 12, 7, "zhangsan", "bot")
	if err != nil {
		t.Fatalf("EnsureNameUnique: %v", err)
	}
	if got != "zhangsan-bot-2" {
		t.Fatalf("消歧名不符：%q", got)
	}
	// -2 也被占 → -3。
	fk.Provision(ctx, 3, "zhangsan-bot-2", "cipher-3")
	got, err = s.EnsureNameUnique(ctx, 12, 7, "zhangsan", "bot")
	if err != nil || got != "zhangsan-bot-3" {
		t.Fatalf("二级消歧不符：%q err=%v", got, err)
	}
	// 令牌 2 改名腾出基础名；本令牌 7 自己占着它 + legacy（excludeTokenID 应全豁免）。
	fk.Provision(ctx, 7, "zhangsan-bot", "cipher-7")
	_, _ = fk.RenameAndActivate(ctx, 7, "ls-token-7", "zhangsan-bot", "cipher-7")
	_, _ = fk.RenameAndActivate(ctx, 2, "zhangsan-bot", "zhangsan-bot-final", "cipher-2")
	if err := fk.ClearLegacyName(ctx, 2); err != nil {
		t.Fatalf("clear legacy: %v", err)
	}
	got, err = s.EnsureNameUnique(ctx, 12, 7, "zhangsan", "bot")
	if err != nil || got != "zhangsan-bot" {
		t.Fatalf("本令牌名不应触发消歧：%q err=%v", got, err)
	}
}

// TestEnsureNameUnique_Exhausted 重试一轮仍撞 → ErrNameUnavailable（契约：409 挂起人工）。
func TestEnsureNameUnique_Exhausted(t *testing.T) {
	s := newSyncForTest(t, mustFakePage(t), newFakeKeyStore())
	fk := s.keys.(*fakeKeyStore)
	// 注入「全部候选都被占」。
	for i := 0; i < maxDisambiguationTries+1; i++ {
		name := "zhangsan-bot"
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i+1)
		}
		fk.occupied[name] = true
	}
	got, err := s.EnsureNameUnique(context.Background(), 12, 7, "zhangsan", "bot")
	if got != "" || !errors.Is(err, ErrNameUnavailable) {
		t.Fatalf("期望 ErrNameUnavailable：got=%q err=%v", got, err)
	}
}

// TestRollbackToken_RestoresLegacyName 回滚主链：key_name=新名、legacy=旧名 →
// 复原后 key_name=旧名、legacy 清空，代理侧旧名可用（值不变）。
func TestRollbackToken_RestoresLegacyName(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	// 预置「已改名」终态：新名 zhangsan-bot，legacy=ls-token-7。
	cipher, err := encryptForTest("secret-xyz")
	if err != nil {
		t.Fatalf("sm4: %v", err)
	}
	fk.Provision(context.Background(), 7, "zhangsan-bot", cipher)
	if _, err := fk.RenameAndActivate(context.Background(), 7, "ls-token-7", "zhangsan-bot", cipher); err != nil {
		t.Fatalf("preset rename: %v", err)
	}
	if _, err := NewPageAdmin(srv.URL, syncTestPageKey, nil).EnsureCallerKey(context.Background(), "zhangsan-bot", "secret-xyz"); err != nil {
		t.Fatalf("preset page: %v", err)
	}
	s := newSyncForTest(t, srv.URL, fk)

	done, err := s.RollbackToken(context.Background(), 7)
	if err != nil || !done {
		t.Fatalf("RollbackToken: done=%v err=%v", done, err)
	}
	k, _ := fk.ByTokenID(context.Background(), 7)
	if k.KeyName != "ls-token-7" || k.LegacyKeyName != "" || k.State != KeyStateActive {
		t.Fatalf("回滚终态不符：%+v", k)
	}
	if v, ok := pageKeys.Load("ls-token-7"); !ok || v != "secret-xyz" {
		t.Fatalf("代理侧旧名应复原：ok=%v v=%v", ok, v)
	}
	if len(fk.legacyClear) != 1 || fk.legacyClear[0] != 7 {
		t.Fatalf("legacy 清理调用不符：%v", fk.legacyClear)
	}
}

// TestRollbackToken_IdempotentRerun 回滚重跑幂等：第二次命中「已复原」直接返回，
// rename 序列与 clear 序列不再增长。
func TestRollbackToken_IdempotentRerun(t *testing.T) {
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	cipher, _ := encryptForTest("secret-xyz")
	fk.Provision(context.Background(), 7, "zhangsan-bot", cipher)
	_, _ = fk.RenameAndActivate(context.Background(), 7, "ls-token-7", "zhangsan-bot", cipher)
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()

	if _, err := s.RollbackToken(ctx, 7); err != nil {
		t.Fatalf("first: %v", err)
	}
	n1, c1 := len(fk.renames), len(fk.legacyClear)
	for i := 0; i < 3; i++ {
		done, err := s.RollbackToken(ctx, 7)
		if err != nil {
			t.Fatalf("rerun %d: err=%v", i, err)
		}
		if done {
			t.Fatalf("rerun %d: 已复原行不应再报 done=true（legacy 已清空=无可回滚）", i)
		}
	}
	if len(fk.renames) != n1 || len(fk.legacyClear) != c1 {
		t.Fatalf("重跑产生了新动作：renames=%d clear=%d", len(fk.renames), len(fk.legacyClear))
	}
	// 终态复核：key_name=旧名、legacy 清空、ACTIVE。
	k, _ := fk.ByTokenID(ctx, 7)
	if k.KeyName != "ls-token-7" || k.LegacyKeyName != "" || k.State != KeyStateActive {
		t.Fatalf("重跑后终态漂移：%+v", k)
	}
}

// TestRollbackToken_NoLegacySkips 无旧名（未改名过/无映射）→ 幂等跳过，不报错。
func TestRollbackToken_NoLegacySkips(t *testing.T) {
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "zhangsan-bot", "cipher")
	s := newSyncForTest(t, srv.URL, fk)
	done, err := s.RollbackToken(context.Background(), 7)
	if err != nil || done {
		t.Fatalf("无旧名应跳过：done=%v err=%v", done, err)
	}
	done, err = s.RollbackToken(context.Background(), 999) // 无映射
	if err != nil || done {
		t.Fatalf("无映射应跳过：done=%v err=%v", done, err)
	}
}

// mustFakePage 起一个空 fake magpie 页面（消歧用例不需要代理侧数据）。
func mustFakePage(t *testing.T) string {
	t.Helper()
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	return srv.URL
}

var _ = strings.TrimSpace // 保持 import 稳定（防用例删减后 import 失配）
