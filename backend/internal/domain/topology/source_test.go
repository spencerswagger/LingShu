package topology

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

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
