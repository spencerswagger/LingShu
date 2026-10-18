package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// fakeProtocols 是 ProtocolResolver 的测试实现：固定返回同一渠道协议。
type fakeProtocols struct {
	cp  *channelprotocol.ChannelProtocol
	err error
}

func (f *fakeProtocols) GetByID(_ context.Context, _ int64) (*channelprotocol.ChannelProtocol, error) {
	return f.cp, f.err
}

// statRec 记录一次协议级错误统计写入。
type statRec struct {
	protocolID int64
	category   string
	day        time.Time
}

// fakeStats 是 ProtocolErrorStatsRecorder 的测试实现。
type fakeStats struct{ recs []statRec }

func (f *fakeStats) IncrProtocolErrorStat(_ context.Context, protocolID int64, category string, day time.Time) error {
	f.recs = append(f.recs, statRec{protocolID: protocolID, category: category, day: day})
	return nil
}

// upstreamErrServer 返回固定 HTTP 状态与错误体的上游服务。
func upstreamErrServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withPolicy 给 harness 接入渠道协议（含错误策略）与统计写入 fake。
func withPolicy(h *harness, protocolID int64, policies []channelprotocol.ErrorPolicy) *fakeStats {
	cp := &channelprotocol.ChannelProtocol{
		ID: protocolID,
		// 协议须包含入站基础协议，否则模型直通判定（effectiveProtocols）会拒绝该候选。
		Protocols:   channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}},
		ErrorPolicy: policies,
	}
	h.chans.rt.Protocol = cp
	h.gw.protocols = &fakeProtocols{cp: cp}
	stats := &fakeStats{}
	h.gw.protocolStats = stats
	return stats
}

const errBody422 = `{"error":{"message":"context length exceeded","type":"invalid_request_error","code":"context_length_exceeded"}}`

// Step 8：非流式默认分支命中策略（不计入健康 + 不转移 + 自定义下游状态码）。
func TestApplyErrorPolicyNonStreamDisposition(t *testing.T) {
	up := upstreamErrServer(t, http.StatusUnprocessableEntity, errBody422)
	h := build(t, up.URL)
	no := false
	stats := withPolicy(h, 9, []channelprotocol.ErrorPolicy{{
		Scope: channelprotocol.ErrorScopeChannel,
		Match: channelprotocol.ErrorMatch{Status: http.StatusUnprocessableEntity},
		Disposition: channelprotocol.ErrorDisposition{
			CountIntoHealth:  false,
			Failover:         &no,
			Action:           channelprotocol.ErrorActionNone,
			DownstreamStatus: http.StatusUnprocessableEntity,
		},
	}})

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 0 {
		t.Errorf("countIntoHealth=false 不应回喂渠道健康, got %+v", h.chans.feeds)
	}
	if len(stats.recs) != 1 || stats.recs[0].protocolID != 9 || stats.recs[0].category != "status:422" {
		t.Fatalf("协议级错误统计不符: %+v", stats.recs)
	}
}

// Step 9：流式默认分支命中策略（与非流式同构）。
func TestApplyErrorPolicyStreamDisposition(t *testing.T) {
	up := upstreamErrServer(t, http.StatusUnprocessableEntity, errBody422)
	h := build(t, up.URL)
	no := false
	withPolicy(h, 9, []channelprotocol.ErrorPolicy{{
		Scope: channelprotocol.ErrorScopeChannel,
		Match: channelprotocol.ErrorMatch{Status: http.StatusUnprocessableEntity},
		Disposition: channelprotocol.ErrorDisposition{
			CountIntoHealth:  false,
			Failover:         &no,
			DownstreamStatus: http.StatusUnprocessableEntity,
		},
	}})

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":true}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("stream want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.chans.feeds) != 0 {
		t.Errorf("stream countIntoHealth=false 不应回喂, got %+v", h.chans.feeds)
	}
}

// Step 8：Failover 显式 true 覆盖默认（400 本不可转移，策略改为可转移 → 单候选耗尽后 503）。
func TestApplyErrorPolicyFailoverOverride(t *testing.T) {
	up := upstreamErrServer(t, http.StatusBadRequest, `{"error":{"message":"bad request"}}`)
	h := build(t, up.URL)
	yes := true
	withPolicy(h, 9, []channelprotocol.ErrorPolicy{{
		Scope: channelprotocol.ErrorScopeChannel,
		Match: channelprotocol.ErrorMatch{Status: http.StatusBadRequest},
		Disposition: channelprotocol.ErrorDisposition{
			CountIntoHealth: true,
			Failover:        &yes,
		},
	}})

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failover=true 应转移并最终 503，got %d: %s", rec.Code, rec.Body.String())
	}
}

// Step 8：Action=drain 触发手动置密钥/模型状态。
func TestApplyErrorPolicyActionDrain(t *testing.T) {
	up := upstreamErrServer(t, http.StatusUnprocessableEntity, errBody422)
	h := build(t, up.URL)
	no := false
	withPolicy(h, 9, []channelprotocol.ErrorPolicy{{
		Scope: channelprotocol.ErrorScopeChannel,
		Match: channelprotocol.ErrorMatch{Status: http.StatusUnprocessableEntity},
		Disposition: channelprotocol.ErrorDisposition{
			CountIntoHealth: false,
			Failover:        &no,
			Action:          channelprotocol.ErrorActionDrain,
		},
	}})

	_ = doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if len(h.chans.keyActions) != 1 || h.chans.keyActions[0] != channel.ActionDrain {
		t.Fatalf("want key action=%s, got %+v", channel.ActionDrain, h.chans.keyActions)
	}
	if len(h.chans.modelStates) != 1 || h.chans.modelStates[0] != channel.StateDrain {
		t.Fatalf("want model state=%s, got %+v", channel.StateDrain, h.chans.modelStates)
	}
}

// Step 10：命中策略时按 errorCode 归出类别并累加协议级统计（code:<errCode>）。
func TestApplyErrorPolicyStatsByCode(t *testing.T) {
	up := upstreamErrServer(t, http.StatusUnprocessableEntity, errBody422)
	h := build(t, up.URL)
	no := false
	stats := withPolicy(h, 9, []channelprotocol.ErrorPolicy{{
		Scope: channelprotocol.ErrorScopeChannel,
		Match: channelprotocol.ErrorMatch{Status: http.StatusUnprocessableEntity, ErrorCode: "context_length_exceeded"},
		Disposition: channelprotocol.ErrorDisposition{
			CountIntoHealth: false,
			Failover:        &no,
		},
	}})

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusBadGateway { // 未设 downstreamStatus → 默认 502
		t.Fatalf("want 502, got %d", rec.Code)
	}
	if len(stats.recs) != 1 || stats.recs[0].category != "code:context_length_exceeded" {
		t.Fatalf("want category=code:context_length_exceeded, got %+v", stats.recs)
	}
}

// Step 8：未装配协议/未配置策略时保持改造前行为（504 → 502 且照常回喂健康）。
func TestApplyErrorPolicyDefaultSafety(t *testing.T) {
	up := upstreamErrServer(t, http.StatusGatewayTimeout, `{"error":{"message":"timeout"}}`)
	h := build(t, up.URL)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("default want 502, got %d", rec.Code)
	}
	if len(h.chans.feeds) != 1 {
		t.Fatalf("default 应照常回喂健康一次, got %+v", h.chans.feeds)
	}
}
