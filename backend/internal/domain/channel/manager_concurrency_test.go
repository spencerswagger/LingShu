package channel

import (
	"sync"
	"testing"
	"time"
)

// TestManager_ConcurrentKeyStateReadWrite 并发读密钥状态（GetKeyRuntime → Machine.State）
// 与并发写（FeedResult/ForceKeyNormal/ManualSetKeyState，均在 Manager.mu 内写 Machine.state）。
// 用于验证 P2：Machine.state 原子化后，Manager.mu 之外的读与锁内的写不再构成 data race。
// 配合 `go test -race ./internal/domain/channel/` 运行。
func TestManager_ConcurrentKeyStateReadWrite(t *testing.T) {
	mgr := NewManager(nil, nil, make([]byte, 16), nil, nil, nil)
	mgr.Upsert(&Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		RateLimit: RateLimitConfig{MaxConcurrent: 8},
	})
	mgr.UpsertKey(ChannelKey{ID: 11, ChannelID: 1, Name: "k-a", State: StateNormal})
	mgr.UpsertKey(ChannelKey{ID: 12, ChannelID: 1, Name: "k-b", State: StateNormal})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读方：模拟路由热路径经 GetKeyRuntime 取状态（Manager.mu 之外读 Machine.State）。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if kr, ok := mgr.GetKeyRuntime(11); ok {
					_ = kr.Machine.State()
				}
				_ = mgr.KeyOrderForRouting(1)
			}
		}()
	}

	// 写方：状态机回喂与手动切换（均持 Manager.mu 写 Machine.state）。
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				mgr.FeedResult(11, 0, Feedback{IsSuccess: j%2 == 0})
				mgr.FeedResult(12, 0, Feedback{Is429: j%5 == 0})
				if j%7 == 0 {
					_ = mgr.ManualSetKeyState(11, "drain")
					mgr.ForceKeyNormal(11)
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
