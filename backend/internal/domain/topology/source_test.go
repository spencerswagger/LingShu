package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// rname 生成一个随机短名（测试组名的随机驱动：归一规则必须对任意名字成立）。
func rname() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// 网关字段有两套命名（OpenAI 兼容层 + 网关自有），这里覆盖「多别名」与「组接口不可用不致命」。
func TestHTTPGatewaySource_Fetch_ParsesAliases(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer admin-key" {
			t.Errorf("上游鉴权头 = %q，期望 Bearer admin-key", got)
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"acme/gpt-x","owned_by":"acme","context_length":128000,
			 "capabilities":{"tools":true},"pricing":{"input":1}},
			{"model":"borealis/claude-y","provider":"borealis","context_len":200000},
			{"display":"无 id 的坏数据"}
		]}`))
	})
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"groups":[
			{"name":"team-2","display_name":"Team 2"},
			{"name":"auto"},
			{"name":"auto-fast"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src := NewHTTPGatewaySource(srv.URL, "admin-key", "", srv.Client())
	nodes, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch 失败：%v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("节点数 = %d，期望 3（2 个模型 + 1 个组；auto 组被过滤、无 id 的丢弃）：%+v", len(nodes), nodes)
	}
	byRef := map[string]Node{}
	for _, n := range nodes {
		byRef[n.Ref] = n
	}
	x, ok := byRef["acme/gpt-x"]
	if !ok {
		t.Fatalf("缺少 acme/gpt-x：%+v", nodes)
	}
	if x.ProviderRef != "acme" || x.Kind != KindDirect || x.ContextLen != 128000 {
		t.Errorf("acme/gpt-x 解析错误：%+v", x)
	}
	if string(x.Caps) != `{"tools":true}` {
		t.Errorf("caps 应原样搬运，得到 %s", x.Caps)
	}
	y := byRef["borealis/claude-y"]
	if y.ProviderRef != "borealis" || y.ContextLen != 200000 {
		t.Errorf("别名取值错误：%+v", y)
	}
	g := byRef["group/team-2"]
	if g.Kind != KindGroup || g.DisplayName != "Team 2" || g.ProviderRef != "" {
		t.Errorf("组节点解析错误：%+v", g)
	}
}

// 组接口 404 不影响模型层结论（组只是四层图的一层，缺了不该让整轮失败）。
func TestHTTPGatewaySource_Fetch_GroupsUnavailableIsNotFatal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"acme/gpt-x"}]`)) // 裸数组也是合法外壳
	})
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	nodes, err := NewHTTPGatewaySource(srv.URL, "", "", srv.Client()).Fetch(context.Background())
	if err != nil {
		t.Fatalf("组接口 404 不应让整轮失败：%v", err)
	}
	if len(nodes) != 1 || nodes[0].Ref != "acme/gpt-x" {
		t.Fatalf("节点 = %+v，期望只有 acme/gpt-x", nodes)
	}
}

// 模型接口非 2xx 必须整轮失败，且错误里带状态码与响应片段（排障靠它）。
func TestHTTPGatewaySource_Fetch_ModelErrorIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := NewHTTPGatewaySource(srv.URL, "", "", srv.Client()).Fetch(context.Background())
	if err == nil {
		t.Fatal("401 应当返回错误")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("错误信息缺少状态码/响应片段：%v", err)
	}
}

// 未配基址 → ErrSourceNotConfigured（不是「同步 0 条成功」）。
func TestHTTPGatewaySource_Fetch_NotConfigured(t *testing.T) {
	_, err := NewHTTPGatewaySource("  ", "", "", nil).Fetch(context.Background())
	if err != ErrSourceNotConfigured {
		t.Fatalf("err = %v，期望 ErrSourceNotConfigured", err)
	}
	if NewHTTPGatewaySource("  ", "", "", nil).Configured() {
		t.Fatal("空基址不应判定为已配置")
	}
}

// 路径可被环境变量覆盖（真实网关路径与默认值不一致时不用发版）。
func TestHTTPGatewaySource_PathOverrideFromEnv(t *testing.T) {
	t.Setenv("TOPOLOGY_MODELS_PATH", "/gw/models")
	t.Setenv("TOPOLOGY_GROUPS_PATH", "/gw/groups")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gw/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"acme/gpt-x"}]}`))
	}))
	defer srv.Close()

	nodes, err := NewHTTPGatewaySource(srv.URL, "", "", srv.Client()).Fetch(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("路径覆盖未生效：nodes=%+v err=%v", nodes, err)
	}
	_ = os.Unsetenv("TOPOLOGY_MODELS_PATH")
}

func TestParseModels_BadJSON(t *testing.T) {
	if _, err := parseModels([]byte(`not json`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestParseGroups_AutoFiltered(t *testing.T) {
	groups, err := parseGroups([]byte(`[{"name":"g1"},{"id":"auto"},{"name":"Auto-Group"}]`))
	if err != nil {
		t.Fatalf("parseGroups 失败：%v", err)
	}
	if len(groups) != 1 || groups[0].Ref != "group/g1" {
		raw, _ := json.Marshal(groups)
		t.Fatalf("auto 组未被过滤：%s", raw)
	}
}

// 组 ref 必须与账本 req_model 同形态（group/<id>）：绑定真源（external_models.bound_internal_ref）
// 与结算侧都按这个形态匹配。历史上「优先用 magpie 的 ref」出过两种坏形态：
//   - 用户组 ref="group/team-a" 再拼一层 "group/" → "group/group/team-a"（双前缀，永远匹配不到）；
//   - auto 组 ref="auto/flash" → "group/auto/flash"（错误形态，且绕过了 auto 组过滤）。
//
// 随机组名都要按同一规则归一，禁止对任何具体名字特判。
func TestParseGroups_CanonicalRef(t *testing.T) {
	team, auto, legacy, dup, noid := rname(), rname(), rname(), rname(), rname()
	raw := fmt.Sprintf(`[
		{"id":"team-%s","ref":"group/team-%s","display_name":"Team %s"},
		{"id":"auto-%s","ref":"auto/%s","display_name":"Auto %s"},
		{"name":"legacy-%s"},
		{"ref":"group/dup-%s"},
		{"ref":"auto/%s","display_name":"无 id 的 auto 组"}
	]`, team, team, team, auto, auto, auto, legacy, dup, noid)
	groups, err := parseGroups([]byte(raw))
	if err != nil {
		t.Fatalf("parseGroups 失败：%v", err)
	}
	// auto 组（id 与 ref 两种形态）都要被过滤；剩下的组 ref 必须是 group/<id> 形态。
	if len(groups) != 3 {
		b, _ := json.Marshal(groups)
		t.Fatalf("组数 = %d，期望 3：%s", len(groups), b)
	}
	byRef := map[string]Node{}
	for _, n := range groups {
		if !strings.HasPrefix(n.Ref, "group/") {
			t.Fatalf("组 ref 不是 group/ 形态：%+v", n)
		}
		if strings.HasPrefix(strings.TrimPrefix(n.Ref, "group/"), "group/") {
			t.Fatalf("组 ref 双前缀：%+v", n)
		}
		byRef[n.Ref] = n
	}
	if _, ok := byRef["group/team-"+team]; !ok {
		t.Fatalf("用户组 ref 应为 group/<id>（原始 ref 已是同形态，不得双前缀）：%+v", groups)
	}
	if _, ok := byRef["group/legacy-"+legacy]; !ok {
		t.Fatalf("只有 name 的组应回落 group/<name>：%+v", groups)
	}
	if _, ok := byRef["group/dup-"+dup]; !ok {
		t.Fatalf("裸 ref 组应原样归一（不重复加前缀）：%+v", groups)
	}
}
