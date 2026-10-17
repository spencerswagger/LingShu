package router

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestSessionRegistry_ConcurrentCountActive 并发读 CountActive（读锁）与并发写
// Attach/Close（写锁），验证 P1 将 mu 改为 RWMutex 后读写计数无 data race。
// 配合 `go test -race ./internal/domain/router/` 运行。
func TestSessionRegistry_ConcurrentCountActive(t *testing.T) {
	r := NewSessionRegistry(nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

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
				_ = r.CountActive(1)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("s-%d-%d", seed, j)
				ok, _ := r.Attach(&Session{
					SessionID: id, ChannelKeyID: 1, InternalModelID: "cn-internal",
					ExpireAt: time.Now().Add(time.Hour),
				}, 1<<30)
				if ok {
					r.Close(id)
				}
				_ = r.CountActive(1)
			}
		}(i)
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
