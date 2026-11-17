package topology

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeSource 是可控的数据源：返回固定快照或固定错误，可阻塞以模拟慢同步。
type fakeSource struct {
	nodes []Node
	err   error
	block chan struct{} // 非 nil 时 Fetch 阻塞到该 channel 关闭
}

func (f *fakeSource) Fetch(ctx context.Context) ([]Node, error) {
	if f.block != nil {
		<-f.block
	}
	return f.nodes, f.err
}

// fakeStore 记录同步器对它做了什么（Replace 有没有被调用是「失败不清空快照」的直接证据）。
type fakeStore struct {
	mu        sync.Mutex
	replaceN  int
	nodes     []Node
	links     []Link
	counts    Counts
	lastSeen  time.Time
	hasSeen   bool
	state     SyncState
	replaceFn func(nodes []Node, runAt time.Time) (int, int, error)
	saveErr   error
}

func (f *fakeStore) Nodes(ctx context.Context) ([]Node, error)  { return f.nodes, nil }
func (f *fakeStore) Links(ctx context.Context) ([]Link, error)  { return f.links, nil }
func (f *fakeStore) Counts(ctx context.Context) (Counts, error) { return f.counts, nil }
func (f *fakeStore) LastSeenAt(ctx context.Context) (time.Time, bool, error) {
	return f.lastSeen, f.hasSeen, nil
}

func (f *fakeStore) Replace(ctx context.Context, nodes []Node, runAt time.Time) (int, int, error) {
	f.mu.Lock()
	f.replaceN++
	fn := f.replaceFn
	f.mu.Unlock()
	if fn != nil {
		return fn(nodes, runAt)
	}
	return len(nodes), 0, nil
}

func (f *fakeStore) LoadState(ctx context.Context) (SyncState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeStore) SaveState(ctx context.Context, st SyncState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.state = st
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// 成功路径：Replace 被调用一次，状态写 ok 并清空 Message（失败原因自上次成功起就无意义）。
func TestSyncer_RunOnce_Success(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	st := &fakeStore{state: SyncState{LastRunAt: now.Add(-time.Hour), LastSuccessAt: now.Add(-time.Hour),
		State: StateFailed, Message: "上次失败"}}
	src := &fakeSource{nodes: []Node{{Ref: "acme/gpt-x", Kind: KindDirect}}}
	sy := NewSyncer(SyncerOptions{Source: src, Store: st, Now: fixedNow(now), Logger: testLogger()})

	res, err := sy.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce 失败：%v", err)
	}
	if res.Fetched != 1 || res.Created != 1 || res.State != StateOK {
		t.Fatalf("结果 = %+v", res)
	}
	if st.replaceN != 1 {
		t.Fatalf("Replace 调用次数 = %d，期望 1", st.replaceN)
	}
	if st.state.State != StateOK || st.state.Message != "" || !st.state.LastSuccessAt.Equal(now) {
		t.Fatalf("落库状态 = %+v", st.state)
	}
}

// 取数失败：不许碰镜像（Replace 不调用），状态记 failed 且保留上次成功时间。
func TestSyncer_RunOnce_FetchErrorKeepsSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	lastOK := now.Add(-2 * time.Hour)
	st := &fakeStore{state: SyncState{LastSuccessAt: lastOK, State: StateOK}}
	src := &fakeSource{err: errors.New("网关 502")}
	sy := NewSyncer(SyncerOptions{Source: src, Store: st, Now: fixedNow(now), Logger: testLogger()})

	res, err := sy.RunOnce(context.Background())
	if err == nil {
		t.Fatal("取数失败必须返回错误")
	}
	if st.replaceN != 0 {
		t.Fatalf("失败时不许写入镜像，Replace 调用次数 = %d", st.replaceN)
	}
	if st.state.State != StateFailed || st.state.Message != "网关 502" {
		t.Fatalf("失败状态 = %+v", st.state)
	}
	if !st.state.LastSuccessAt.Equal(lastOK) {
		t.Fatalf("失败时 LastSuccessAt 必须保留 = %v，得到 %v", lastOK, st.state.LastSuccessAt)
	}
	if res.State != StateFailed || res.Message != "网关 502" {
		t.Fatalf("返回结果 = %+v", res)
	}
}

// 空快照 = 失败：否则会把存量节点集体标灰（页面全灰比报错更容易被忽略）。
func TestSyncer_RunOnce_EmptySnapshotIsFailure(t *testing.T) {
	st := &fakeStore{}
	sy := NewSyncer(SyncerOptions{Source: &fakeSource{}, Store: st, Logger: testLogger()})

	_, err := sy.RunOnce(context.Background())
	if !errors.Is(err, ErrEmptySnapshot) {
		t.Fatalf("err = %v，期望 ErrEmptySnapshot", err)
	}
	if st.replaceN != 0 {
		t.Fatalf("空快照不许写镜像，Replace 调用次数 = %d", st.replaceN)
	}
}

// 写状态失败不许把成功改成失败：数据已落库，只是状态没记上（要留 ERROR 但不是业务失败）。
func TestSyncer_RunOnce_SaveStateErrorStillReturnsOK(t *testing.T) {
	st := &fakeStore{saveErr: errors.New("sys_configs 写入失败")}
	sy := NewSyncer(SyncerOptions{Source: &fakeSource{nodes: []Node{{Ref: "a"}}}, Store: st, Logger: testLogger()})

	res, err := sy.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("状态写入失败不应让整轮失败：%v", err)
	}
	if res.State != StateOK || st.replaceN != 1 {
		t.Fatalf("res=%+v replaceN=%d", res, st.replaceN)
	}
}

// 同一时刻只跑一轮：第二发直接 ErrSyncInFlight（不排队、不叠加网关请求）。
func TestSyncer_RunOnce_InFlightRejected(t *testing.T) {
	block := make(chan struct{})
	st := &fakeStore{}
	sy := NewSyncer(SyncerOptions{Source: &fakeSource{nodes: []Node{{Ref: "a"}}, block: block},
		Store: st, Logger: testLogger()})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := sy.RunOnce(context.Background()); err != nil {
			t.Errorf("第一轮不应失败：%v", err)
		}
	}()
	// 等第一轮进入 Fetch（持锁）后立刻打第二发。
	time.Sleep(20 * time.Millisecond)
	if _, err := sy.RunOnce(context.Background()); !errors.Is(err, ErrSyncInFlight) {
		t.Fatalf("err = %v，期望 ErrSyncInFlight", err)
	}
	close(block)
	<-done
}
