package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/team/llmgateway/internal/domain/automodel"
	"github.com/team/llmgateway/internal/domain/router"
)

// ===== Auto fakes（复用 handler_test.go 的 fake 风格） =====

// fakeAutoResolver 实现 gateway.AutoResolver：按名返回固定成员列表。
type fakeAutoResolver struct {
	members []automodel.Member
	isAuto  bool
	err     error
	// 记录每次解析入参，用于断言网关以「原始请求名 + token.UserID」调用解析器。
	calls  []string
	owners []*int64
}

func (f *fakeAutoResolver) ResolveMembers(_ context.Context, name string, ownerUserID *int64) ([]automodel.Member, bool, error) {
	f.calls = append(f.calls, name)
	f.owners = append(f.owners, ownerUserID)
	return f.members, f.isAuto, f.err
}

// autoFakeRouter 复用 fakeRouter（Sessions/Route），仅按成员对外模型名返回不同候选，
// 以模拟「A 有候选 / B 无候选」的成员级回落。记录被试路由的对外模型名顺序。
type autoFakeRouter struct {
	*fakeRouter
	byModel   map[string][]*router.ChannelRoute
	modelErrs map[string]error
	asked     []string
}

func (f *autoFakeRouter) RouteAll(_ context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error) {
	f.asked = append(f.asked, spec.ExternalModel)
	if err := f.modelErrs[spec.ExternalModel]; err != nil {
		return nil, err
	}
	return f.byModel[spec.ExternalModel], nil
}

// autoMember 构造一个成员（ExternalName 即成员对外模型名）。
func autoMember(id, extID int64, name string, pos int) automodel.Member {
	return automodel.Member{ID: id, ExternalModelID: extID, ExternalName: name, Enabled: true, Position: pos}
}

// newAutoHarness 在既有 build harness 基础上装配 Auto 解析器 + 成员级路由 fake。
func newAutoHarness(t *testing.T, upstreamURL string, resolver *fakeAutoResolver) (*harness, *autoFakeRouter) {
	t.Helper()
	h := build(t, upstreamURL)
	ar := &autoFakeRouter{
		fakeRouter: h.rrouter,
		byModel:    map[string][]*router.ChannelRoute{},
		modelErrs:  map[string]error{},
	}
	h.gw.router = ar
	h.gw.auto = resolver
	return h, ar
}

// routeFor 构造一个候选路由（不同成员用不同 InternalModelID，便于断言命中者）。
func routeFor(internalModel string, externalModelID, keyID int64) []*router.ChannelRoute {
	return []*router.ChannelRoute{{
		ChannelID:       1,
		ChannelKeyID:    keyID,
		ModelRowID:      7,
		InternalModelID: internalModel,
		ExternalModelID: externalModelID,
	}}
}

// upstreamOK 起一个返回成功 JSON 的上游，捕获收到的 model 字段。
func upstreamOK(modelSeen *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m, ok := body["model"].(string); ok {
			*modelSeen = m
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
}

// 场景 1：命中首个可用成员 A —— 路由以 A 的对外模型名发起、后续链路 model 为 A，
// 计费归属 ExternalModel=A 真名 + AutoModelName=auto。
func TestServeAuto_FirstAvailableMemberChosen(t *testing.T) {
	var upstreamModel string
	upstream := upstreamOK(&upstreamModel)
	defer upstream.Close()

	resolver := &fakeAutoResolver{
		isAuto: true,
		members: []automodel.Member{
			autoMember(1, 101, "model-a", 0),
			autoMember(2, 102, "model-b", 1),
		},
	}
	h, ar := newAutoHarness(t, upstream.URL, resolver)
	ar.byModel["model-a"] = routeFor("internal-a", 101, 1)
	ar.byModel["model-b"] = routeFor("internal-b", 102, 1)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"auto","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// 只试了第一个成员 A。
	if len(ar.asked) != 1 || ar.asked[0] != "model-a" {
		t.Fatalf("should only route member A, asked=%v", ar.asked)
	}
	// 后续链路 model 为 A：上游收到 A 的内部模型 ID。
	if upstreamModel != "internal-a" {
		t.Fatalf("upstream model want internal-a, got %q", upstreamModel)
	}
	// 解析器以原始 Auto 名 + 请求用户调用。
	if len(resolver.calls) != 1 || resolver.calls[0] != "auto" {
		t.Fatalf("resolver calls=%v", resolver.calls)
	}
	if resolver.owners[0] == nil || *resolver.owners[0] != 7 {
		t.Fatalf("resolver ownerUserID want 7, got %v", resolver.owners[0])
	}
	// 计费归属：ExternalModel = A 真名，AutoModelName = auto。
	if len(h.billing.reqs) != 1 {
		t.Fatalf("billing called %d times, want 1", len(h.billing.reqs))
	}
	req := h.billing.reqs[0]
	if req.ExternalModel != "model-a" || req.AutoModelName != "auto" {
		t.Fatalf("billing attribution wrong: external=%q auto=%q", req.ExternalModel, req.AutoModelName)
	}
	if req.Fail {
		t.Fatalf("should be a completed billing, got %+v", req)
	}
}

// 场景 2：首个成员不可用（无候选 / 路由错误）→ 回落到 B。
func TestServeAuto_FallbackToNextMember(t *testing.T) {
	var upstreamModel string
	upstream := upstreamOK(&upstreamModel)
	defer upstream.Close()

	resolver := &fakeAutoResolver{
		isAuto: true,
		members: []automodel.Member{
			autoMember(1, 101, "model-a", 0),
			autoMember(2, 102, "model-b", 1),
		},
	}
	h, ar := newAutoHarness(t, upstream.URL, resolver)
	// A 无候选（空列表），B 有候选。
	ar.byModel["model-a"] = nil
	ar.byModel["model-b"] = routeFor("internal-b", 102, 1)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"auto","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(ar.asked) != 2 || ar.asked[0] != "model-a" || ar.asked[1] != "model-b" {
		t.Fatalf("should try A then B, asked=%v", ar.asked)
	}
	if upstreamModel != "internal-b" {
		t.Fatalf("upstream model want internal-b, got %q", upstreamModel)
	}
	req := h.billing.reqs[0]
	if req.ExternalModel != "model-b" || req.AutoModelName != "auto" {
		t.Fatalf("billing attribution want model-b/auto, got %q/%q", req.ExternalModel, req.AutoModelName)
	}
}

// 场景 2b：首个成员路由返回错误（如该对外模型未配置）→ 亦回落到 B，不阻断。
func TestServeAuto_FirstMemberRouteErrorFallback(t *testing.T) {
	var upstreamModel string
	upstream := upstreamOK(&upstreamModel)
	defer upstream.Close()

	resolver := &fakeAutoResolver{
		isAuto: true,
		members: []automodel.Member{
			autoMember(1, 101, "model-a", 0),
			autoMember(2, 102, "model-b", 1),
		},
	}
	h, ar := newAutoHarness(t, upstream.URL, resolver)
	ar.modelErrs["model-a"] = &router.RouteError{Kind: router.KindModelNotFound, HTTPStatus: http.StatusNotFound, Message: "not found"}
	ar.byModel["model-b"] = routeFor("internal-b", 102, 1)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"auto","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "internal-b" {
		t.Fatalf("upstream model want internal-b, got %q", upstreamModel)
	}
}

// 场景 3：全部成员不可用 → 503；账单 ExternalModel 为空、AutoModelName = auto、Fail=true。
func TestServeAuto_AllMembersUnavailable_503(t *testing.T) {
	h, ar := newAutoHarness(t, "http://unused", &fakeAutoResolver{
		isAuto: true,
		members: []automodel.Member{
			autoMember(1, 101, "model-a", 0),
			autoMember(2, 102, "model-b", 1),
		},
	})
	// 两个成员均无候选。
	ar.byModel["model-a"] = nil
	ar.byModel["model-b"] = nil

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"auto","stream":false}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.billing.reqs) != 1 {
		t.Fatalf("want 1 billing, got %d", len(h.billing.reqs))
	}
	req := h.billing.reqs[0]
	if !req.Fail || req.ExternalModel != "" || req.AutoModelName != "auto" {
		t.Fatalf("reject billing want fail/empty-external/auto, got %+v", req)
	}
	if len(ar.asked) != 2 {
		t.Fatalf("should try both members, asked=%v", ar.asked)
	}
}

// 场景 4：非 Auto 请求 → 零行为差异：走原路由逻辑，model 保持原值，无 Auto 归因。
func TestServeAuto_NonAutoRequestUnchanged(t *testing.T) {
	var upstreamModel string
	upstream := upstreamOK(&upstreamModel)
	defer upstream.Close()

	resolver := &fakeAutoResolver{isAuto: false} // 未命中 Auto
	h, ar := newAutoHarness(t, upstream.URL, resolver)
	ar.byModel["gpt-4"] = routeFor("gpt-4-internal", 1, 1)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(ar.asked) != 1 || ar.asked[0] != "gpt-4" {
		t.Fatalf("non-auto should route original name once, asked=%v", ar.asked)
	}
	if upstreamModel != "gpt-4-internal" {
		t.Fatalf("upstream model want gpt-4-internal, got %q", upstreamModel)
	}
	req := h.billing.reqs[0]
	if req.ExternalModel != "gpt-4" || req.AutoModelName != "" {
		t.Fatalf("non-auto billing want gpt-4/empty-auto, got %q/%q", req.ExternalModel, req.AutoModelName)
	}
}

// 场景 4b：未装配 Auto 解析器（auto=nil）时行为与改动前一致（不调解析器）。
func TestServeAuto_ResolverNil(t *testing.T) {
	var upstreamModel string
	upstream := upstreamOK(&upstreamModel)
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 保持 gw.auto=nil，仅用成员级路由返回单候选。
	ar := &autoFakeRouter{fakeRouter: h.rrouter, byModel: map[string][]*router.ChannelRoute{}, modelErrs: map[string]error{}}
	ar.byModel["gpt-4"] = routeFor("gpt-4-internal", 1, 1)
	h.gw.router = ar

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", `{"model":"gpt-4","stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if upstreamModel != "gpt-4-internal" {
		t.Fatalf("upstream model want gpt-4-internal, got %q", upstreamModel)
	}
	if h.billing.reqs[0].AutoModelName != "" {
		t.Fatalf("auto model name should be empty, got %q", h.billing.reqs[0].AutoModelName)
	}
}
