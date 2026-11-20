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

	"github.com/team/llmgateway/internal/domain/identity"
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

// TestProbeName_ThreeBranches dry-run 冲突预演的三分支（评审必改：dry-run 探
// Conflict 的数据源）：终态名原样返回（零消歧）；旧自动名走与 Ensure 相同的消歧
// 分配；消歧耗尽返回 ErrNameUnavailable（=identity.ErrCallerKeyNameTaken 哨兵同源）。
func TestProbeName_ThreeBranches(t *testing.T) {
	s := newSyncForTest(t, mustFakePage(t), newFakeKeyStore())
	fk := s.keys.(*fakeKeyStore)
	cipher, err := encryptForTest("secret-p")
	if err != nil {
		t.Fatalf("sm4: %v", err)
	}
	// 分支①：终态名（手工绑定）→ 原样返回，不参与消歧（名字已固化）。
	fk.Provision(context.Background(), 9, "ops-key", cipher)
	got, perr := s.ProbeName(context.Background(), 12, TokenRef{ID: 9, DisplayName: "机密项目"}, "zhangsan")
	if perr != nil || got != "ops-key" {
		t.Fatalf("终态名应原样返回：got=%q err=%v", got, perr)
	}
	// 分支②：旧自动名 → 消歧分配（基础名空闲 → 基础名）。
	fk.Provision(context.Background(), 7, "ls-token-7", cipher)
	got, perr = s.ProbeName(context.Background(), 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan")
	if perr != nil || got != "zhangsan-bot" {
		t.Fatalf("旧自动名应走消歧分配：got=%q err=%v", got, perr)
	}
	// 分支③：候选占满 → ErrNameUnavailable（与 identity 哨兵同源，dry-run 借此探 Conflict）。
	// 基础名 = u3-w-329d（用户名无 ASCII → u<uid>；令牌名无 ASCII → w-+4hex(tokenID=4)）。
	for i := 0; i < maxDisambiguationTries+1; i++ {
		name := "u3-w-329d"
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i+1)
		}
		fk.occupied[name] = true
	}
	got, perr = s.ProbeName(context.Background(), 3, TokenRef{ID: 4, DisplayName: "机密项目"}, "李四")
	if got != "" || !errors.Is(perr, ErrNameUnavailable) || !errors.Is(perr, identity.ErrCallerKeyNameTaken) {
		t.Fatalf("耗尽应返回哨兵（identity 同源）：got=%q err=%v", got, perr)
	}
}

// TestProbeName_DisabledRejected 停用行预演直接拒绝（自动路径不碰停用行）。
func TestProbeName_DisabledRejected(t *testing.T) {
	s := newSyncForTest(t, mustFakePage(t), newFakeKeyStore())
	fk := s.keys.(*fakeKeyStore)
	fk.Provision(context.Background(), 7, "ls-token-7", "")
	if err := fk.Disable(context.Background(), 7); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, perr := s.ProbeName(context.Background(), 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan")
	if got != "" || !errors.Is(perr, ErrKeyDisabled) {
		t.Fatalf("停用行应拒绝：got=%q err=%v", got, perr)
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

// TestEnsure_DisabledRowUntouched 停用行守卫（评审必改①·洞①锚）：
// 手工名停用行被 Ensure 拒（ErrKeyDisabled），本地行不翻 ACTIVE、代理侧零调用、
// 重复调用无副作用（自动回填每轮都撞同一堵墙，不会推进任何状态）。
func TestEnsure_DisabledRowUntouched(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	// 预置：手工名映射，令牌停用时 Remove 过（代理侧已删、本地 DISABLED）。
	fk.Provision(context.Background(), 9, "ops-manual-key", "old-cipher")
	fk.Disable(context.Background(), 9)
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()

	for i := 0; i < 3; i++ { // 重复调用 = 启动补建多轮重试的形状
		err := s.Ensure(ctx, 3, TokenRef{ID: 9, DisplayName: "x"}, "user3", "secret-9")
		if !errors.Is(err, ErrKeyDisabled) {
			t.Fatalf("第 %d 轮：停用行应被拒（ErrKeyDisabled），got %v", i, err)
		}
	}
	k, _ := fk.ByTokenID(ctx, 9)
	if k.State != KeyStateDisabled || k.KeyName != "ops-manual-key" {
		t.Fatalf("停用行被动了：%+v", k)
	}
	if fk.provision != 1 { // 只有预置那一次，Ensure 未走 Provision
		t.Fatalf("Ensure 不得对停用行 Provision（翻状态）：provision=%d", fk.provision)
	}
	if len(fk.renames) != 0 {
		t.Fatalf("Ensure 不得对停用行改名：%v", fk.renames)
	}
	if mapCount(pageKeys) != 0 {
		t.Fatalf("代理侧零调用（不得重建 key）：%d", mapCount(pageKeys))
	}
}

// TestEnsure_DisabledLegacyRowNoProxyRename 停用 + 旧自动名（评审必改①·洞②锚）：
// 绝不先 rename 代理侧 —— 否则代理侧已改、本地被 state 守卫拒，每轮重试死循环。
func TestEnsure_DisabledLegacyRowNoProxyRename(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "ls-token-7", "old-cipher")
	fk.Disable(context.Background(), 7)
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.Ensure(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); !errors.Is(err, ErrKeyDisabled) {
			t.Fatalf("第 %d 轮：got %v", i, err)
		}
	}
	// 代理侧必须零动作：旧名不存在（Toggle 停用时删过）、新名也绝不能被创建/改名出来。
	if mapCount(pageKeys) != 0 {
		t.Fatalf("停用行不得碰代理侧（rename 会造成本地永远追不上的死循环）：%v", snapshotNames(pageKeys))
	}
	if len(fk.renames) != 0 || fk.provision != 1 {
		t.Fatalf("本地零写：renames=%v provision=%d", fk.renames, fk.provision)
	}
	k, _ := fk.ByTokenID(ctx, 7)
	if k.State != KeyStateDisabled || k.KeyName != "ls-token-7" {
		t.Fatalf("停用行被动了：%+v", k)
	}
}

// TestReactivate_DisabledRowFlipsAndConverges 显式启用（Toggle 启用侧 / 管理端一键开通）：
// 停用行先按**当前名**恢复代理 key + 本地翻 ACTIVE，随后常规收敛（旧自动名 → 改名三步），
// 终态与从未停用过的行一致。
func TestReactivate_DisabledRowFlipsAndConverges(t *testing.T) {
	srv, pageKeys := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "ls-token-7", "old-cipher")
	fk.Disable(context.Background(), 7)
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()

	if err := s.Reactivate(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	k, _ := fk.ByTokenID(ctx, 7)
	want := KeyNameFor(12, 7, "zhangsan", "bot")
	if k.State != KeyStateActive || k.KeyName != want || k.LegacyKeyName != "ls-token-7" {
		t.Fatalf("显式启用终态不符：%+v want=%s", k, want)
	}
	if v, ok := pageKeys.Load(want); !ok || v != "secret-xyz" {
		t.Fatalf("代理侧终态不符：ok=%v v=%v", ok, v)
	}
	// 幂等：再跑一次零写收敛。
	p1, r1 := fk.provision, len(fk.renames)
	if err := s.Reactivate(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); err != nil {
		t.Fatalf("Reactivate rerun: %v", err)
	}
	if fk.provision != p1 || len(fk.renames) != r1 {
		t.Fatalf("重跑产生了新写动作：provision=%d→%d renames=%d→%d", p1, fk.provision, r1, len(fk.renames))
	}
}

// TestReactivate_ActiveRowIsPlainEnsure 未停用的行：Reactivate 与 Ensure 等价（终态快检零写）。
func TestReactivate_ActiveRowIsPlainEnsure(t *testing.T) {
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "ls-token-7", "old-cipher")
	s := newSyncForTest(t, srv.URL, fk)
	ctx := context.Background()

	if err := s.Reactivate(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); err != nil {
		t.Fatalf("Reactivate: %v", err)
	}
	want := KeyNameFor(12, 7, "zhangsan", "bot")
	k, _ := fk.ByTokenID(ctx, 7)
	if k.State != KeyStateActive || k.KeyName != want {
		t.Fatalf("终态不符：%+v", k)
	}
	// 再跑：终态快检零写。
	p1, r1 := fk.provision, len(fk.renames)
	if err := s.Reactivate(ctx, 12, TokenRef{ID: 7, DisplayName: "bot"}, "zhangsan", "secret-xyz"); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if fk.provision != p1 || len(fk.renames) != r1 {
		t.Fatalf("终态重跑零写：provision=%d→%d renames=%d→%d", p1, fk.provision, r1, len(fk.renames))
	}
}

// snapshotNames 取 fake 代理侧当前全部名字（失败信息用）。
func snapshotNames(m *sync.Map) []string {
	var out []string
	m.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
	return out
}
