package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// 默认列举端点，相对网关基址。
//
// ⚠️ 契约状态（必须知道，改这里之前先看）：网关侧的拓扑列举接口在本仓库只有口径描述
// （《2026-11-15-v2-upgrade》§「internal_models 的来源」= 网关 `/v1/models` + `/api/groups`），
// 没有逐字段契约。所以本文件按**容错解析**实现：多别名取值、缺字段回落、组接口不可用则不致命。
// 真实网关字段若有出入，只改这个文件里的 mapModel/mapGroup，不动同步器与视图。
// 路径可用 TOPOLOGY_MODELS_PATH / TOPOLOGY_GROUPS_PATH 覆盖，避免为了一个路径发版。
const (
	DefaultModelsPath = "/v1/models"
	DefaultGroupsPath = "/api/groups"

	fetchTimeout = 20 * time.Second
	maxBodyBytes = 8 << 20 // 8 MiB：模型清单不会更大，超过即当作异常响应
)

// Source 是拓扑同步的数据源。单测用它注入固定快照，生产用 HTTPGatewaySource。
type Source interface {
	Fetch(ctx context.Context) ([]Node, error)
}

// HTTPGatewaySource 从网关拉「模型 + 组」清单。
type HTTPGatewaySource struct {
	baseURL    string
	adminKey   string
	authHeader string
	modelsPath string
	groupsPath string
	webKey     string // magpie 页面口令：/api/* 属页面接口，需要它（网关 key 不够）
	webBase    string // 页面端口基址（magpie web，默认 3430）；/api/* 走这里，3425 只有 /v1
	client     *http.Client
}

// NewHTTPGatewaySource 装配网关拓扑源。baseURL 为空时 Fetch 返回 ErrSourceNotConfigured
// （而不是「成功同步 0 条」——那会把存量节点全标 stale）。
func NewHTTPGatewaySource(baseURL, adminKey, authHeader string, client *http.Client) *HTTPGatewaySource {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	if strings.TrimSpace(authHeader) == "" {
		authHeader = "Authorization"
	}
	modelsPath := strings.TrimSpace(os.Getenv("TOPOLOGY_MODELS_PATH"))
	if modelsPath == "" {
		modelsPath = DefaultModelsPath
	}
	groupsPath := strings.TrimSpace(os.Getenv("TOPOLOGY_GROUPS_PATH"))
	if groupsPath == "" {
		groupsPath = DefaultGroupsPath
	}
	webBase := strings.TrimRight(strings.TrimSpace(os.Getenv("MAGPIE_WEB_BASE_URL")), "/")
	// 只有看起来像 URL 才用；compose 变量没插值时会是一串 "${...}"，那种直接忽略。
	if !strings.HasPrefix(webBase, "http://") && !strings.HasPrefix(webBase, "https://") {
		webBase = "http://magpie:3430"
	}
	webKey := strings.TrimSpace(os.Getenv("MAGPIE_WEB_KEY"))
	if webKey == "" {
		if f := strings.TrimSpace(os.Getenv("MAGPIE_WEB_KEY_FILE")); f != "" {
			if b, err := os.ReadFile(f); err == nil {
				webKey = strings.TrimSpace(string(b))
			}
		}
	}
	return &HTTPGatewaySource{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		adminKey:   strings.TrimSpace(adminKey),
		authHeader: authHeader,
		modelsPath: modelsPath,
		groupsPath: groupsPath,
		webKey:     webKey,
		webBase:    webBase,
		client:     client,
	}
}

// Configured 表示是否配了基址（未配置时定时轮不该起，T3 直接给出可读原因）。
func (s *HTTPGatewaySource) Configured() bool {
	return s != nil && s.baseURL != "" && s.client != nil
}

// Fetch 拉取一轮快照：模型清单失败 = 整轮失败（保留上期快照）；
// 组清单失败（404/405 或解析失败）= 只要模型清单成功就继续，组缺失只影响 group 节点。
func (s *HTTPGatewaySource) Fetch(ctx context.Context) ([]Node, error) {
	if !s.Configured() {
		return nil, ErrSourceNotConfigured
	}
	modelsRaw, err := s.get(ctx, s.modelsPath)
	if err != nil {
		return nil, err
	}
	models, err := parseModels(modelsRaw)
	if err != nil {
		return nil, fmt.Errorf("解析网关模型清单失败：%w", err)
	}
	nodes := models

	if groupsRaw, gerr := s.get(ctx, s.groupsPath); gerr == nil {
		groups, err := parseGroups(groupsRaw)
		if err != nil {
			// 不致命：组只是四层图里的一层，缺了不该让整轮同步失败。
			// 但也不静默——同步器的 Message 由调用方决定，这里只丢弃该层。
			groups = nil
		}
		nodes = append(nodes, groups...)
	}
	// 去重（同一 ref 出现在模型与组两处时以先到者为准），并保持稳定顺序。
	seen := make(map[string]struct{}, len(nodes))
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Ref == "" {
			continue
		}
		if _, ok := seen[n.Ref]; ok {
			continue
		}
		seen[n.Ref] = struct{}{}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// FetchRawGroups 原样返回网关 /api/groups 的响应体（页面用于账号/路由组/成员的真实视图）。
func (s *HTTPGatewaySource) FetchRawGroups(ctx context.Context) ([]byte, error) {
	if !s.Configured() {
		return nil, ErrSourceNotConfigured
	}
	return s.get(ctx, s.groupsPath)
}

// get 发一次 GET 并读全量响应体；非 2xx 转成含状态码与截断响应体的可读错误
// （排障时最需要的就是「网关到底回了什么」）。
func (s *HTTPGatewaySource) get(ctx context.Context, path string) ([]byte, error) {
	bases := []string{s.baseURL}
	// /api/* 是 magpie 的**页面接口**，跑在 web 端口（默认 3430）；3425 只有 /v1。
	// 容器网优先按服务名，再试宿主机网关（page 端口已发布到宿主），最后退回网关基址。
	if strings.HasPrefix(path, "/api/") {
		cand := []string{s.webBase, "http://host.docker.internal:3430", s.baseURL}
		bases = bases[:0]
		seen := map[string]bool{}
		for _, b := range cand {
			if b != "" && !seen[b] {
				seen[b] = true
				bases = append(bases, b)
			}
		}
	}
	var lastErr error
	for _, base := range bases {
		body, err := s.doGet(ctx, base, path)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *HTTPGatewaySource) doGet(ctx context.Context, base, path string) ([]byte, error) {
	url := base + path
	if s.webKey != "" && strings.HasPrefix(path, "/api/") {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + "k=" + neturl.QueryEscape(s.webKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构建拓扑请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	if s.webKey != "" && strings.HasPrefix(path, "/api/") {
		req.Header.Set("Cookie", "magpie_web_3430="+s.webKey)
	}
	if s.adminKey != "" {
		v := s.adminKey
		if !strings.Contains(v, " ") {
			v = "Bearer " + v
		}
		req.Header.Set(s.authHeader, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求网关 %s 失败：%w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取网关 %s 响应失败：%w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return nil, fmt.Errorf("网关 %s 返回 %d：%s", path, resp.StatusCode, snippet)
	}
	return body, nil
}

// envelope 兼容三种常见外壳：{"data":[...]} / {"models":[...]} 或 {"groups":[...]} / 裸数组。
func envelope(raw []byte, keys ...string) ([]json.RawMessage, error) {
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch v := probe.(type) {
	case []any:
		out := make([]json.RawMessage, 0, len(v))
		for _, it := range v {
			b, err := json.Marshal(it)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, nil
	case map[string]any:
		for _, k := range keys {
			arr, ok := v[k].([]any)
			if !ok {
				continue
			}
			out := make([]json.RawMessage, 0, len(arr))
			for _, it := range arr {
				b, err := json.Marshal(it)
				if err != nil {
					return nil, err
				}
				out = append(out, b)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("响应既不是数组也没有 %v 字段", keys)
}

type modelItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	OwnedBy  string `json:"owned_by"`
	// 展示名/上下文长度存在多套命名（网关与 OpenAI 兼容层各一套），全部接住。
	DisplayName  string `json:"display_name"`
	DisplayName2 string `json:"display"`
	ContextLen   int64  `json:"context_length"`
	ContextLen2  int64  `json:"context_len"`
	ContextLen3  int64  `json:"max_context_length"`
	// 能力位与牌价原样搬运（换算只在一处，这里不做任何金额运算）。
	Capabilities json.RawMessage `json:"capabilities"`
	Caps         json.RawMessage `json:"caps"`
	Pricing      json.RawMessage `json:"pricing"`
	Price        json.RawMessage `json:"price"`
}

func (m modelItem) ref() string {
	for _, v := range []string{m.ID, m.Model, m.Name} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (m modelItem) provider() string {
	for _, v := range []string{m.Provider, m.OwnedBy} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// 网关的模型 id 约定是 provider/model：取第一段当供应商（只有一段时为空）。
	if i := strings.Index(m.ref(), "/"); i > 0 {
		return m.ref()[:i]
	}
	return ""
}

func (m modelItem) contextLen() int64 {
	for _, v := range []int64{m.ContextLen, m.ContextLen2, m.ContextLen3} {
		if v > 0 {
			return v
		}
	}
	return 0
}

func (m modelItem) displayName() string {
	for _, v := range []string{m.DisplayName, m.DisplayName2} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	// 回落：ref 去掉 provider 前缀。
	r := m.ref()
	if i := strings.Index(r, "/"); i >= 0 && i+1 < len(r) {
		return r[i+1:]
	}
	return r
}

func (m modelItem) caps() json.RawMessage  { return firstJSON(m.Capabilities, m.Caps) }
func (m modelItem) price() json.RawMessage { return firstJSON(m.Pricing, m.Price) }

func firstJSON(vals ...json.RawMessage) json.RawMessage {
	for _, v := range vals {
		if len(v) > 0 && string(v) != "null" {
			return v
		}
	}
	return json.RawMessage(`{}`)
}

func parseModels(raw []byte) ([]Node, error) {
	items, err := envelope(raw, "data", "models", "items")
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(items))
	for _, it := range items {
		var m modelItem
		if err := json.Unmarshal(it, &m); err != nil {
			continue // 单条坏数据不毁整轮
		}
		if m.ref() == "" {
			continue
		}
		out = append(out, Node{
			Ref:         m.ref(),
			ProviderRef: m.provider(),
			Kind:        KindDirect,
			DisplayName: m.displayName(),
			ContextLen:  m.contextLen(),
			Caps:        m.caps(),
			ListPrice:   m.price(),
		})
	}
	return out, nil
}

type groupItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Ref 是 magpie 给组定的对外引用：auto/<model> 或 group/<id>；绑定对外模型用它最稳。
	Ref string `json:"ref"`
}

func (g groupItem) ref() string {
	for _, v := range []string{g.Ref, g.Name, g.ID} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseGroups(raw []byte) ([]Node, error) {
	items, err := envelope(raw, "data", "groups", "items")
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(items))
	for _, it := range items {
		var g groupItem
		if err := json.Unmarshal(it, &g); err != nil {
			continue
		}
		ref := g.ref()
		if ref == "" {
			continue
		}
		// auto 组是网关自动聚合的产物，V2 明确不使用（见 0019 头注），同步侧直接过滤。
		if strings.EqualFold(ref, "auto") || strings.HasPrefix(strings.ToLower(ref), "auto-") {
			continue
		}
		name := strings.TrimSpace(g.DisplayName)
		if name == "" {
			name = ref
		}
		out = append(out, Node{
			Ref:         "group/" + ref,
			ProviderRef: "", // 组是跨供应商的聚合层，不归属单一供应商
			Kind:        KindGroup,
			DisplayName: name,
			Caps:        json.RawMessage(`{}`),
			ListPrice:   json.RawMessage(`{}`),
		})
	}
	return out, nil
}
