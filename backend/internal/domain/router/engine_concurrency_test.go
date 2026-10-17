package router

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/model"
)

// fakeExtModelSource 直接返回固定对外模型（无 DB），供并发用例复用。
type fakeExtModelSource struct{ m model.ExternalModel }

func (f *fakeExtModelSource) GetByExternalName(_ context.Context, _ string) (*model.ExternalModel, error) {
	return &f.m, nil
}

// TestEngine_ConcurrentRouteAndStateFeedback 并发执行 RouteAll 与状态机回喂/强制切换：
// 读方经 GetKeyRuntime/KeyOrderForRouting/Snapshot/ModelViews 读取渠道视图，
// 写方在 Manager.mu 内 Feed/Force 状态机。用于验证 P2（Machine.state 原子化）与
// 相关读路径无 data race（配合 `go test -race ./internal/domain/router/`）。
func TestEngine_ConcurrentRouteAndStateFeedback(t *testing.T) {
	models := []channel.ChannelModel{cm(1, "cn-internal"), cm(2, "us-internal")}
	keys := []channel.ChannelKey{
		newKey(11, 1, channel.StateNormal),
		newKey(12, 1, channel.StateNormal),
		newKey(21, 2, channel.StateNormal),
	}
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, models, keys)

	ext := &fakeExtModelSource{m: model.ExternalModel{ID: 10, ExternalName: "qw-max", Enabled: true}}
	e := NewEngine(ext, &fakeCMSource{cms: models}, mgr, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读方：并发路由（展开候选时会读密钥状态与模型/渠道视图）。
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
				_, _ = e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max"})
			}
		}()
	}

	// 写方：并发回喂/强制切换密钥状态（均在 Manager.mu 内写 Machine.state）。
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
				mgr.FeedResult(11, 0, channel.Feedback{IsSuccess: j%2 == 0})
				mgr.FeedResult(21, 0, channel.Feedback{Is429: j%3 == 0})
				if j%5 == 0 {
					_ = mgr.ManualSetKeyState(12, "drain")
					mgr.ForceKeyNormal(12)
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
