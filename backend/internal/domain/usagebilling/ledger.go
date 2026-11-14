// ledger.go 拉取代理侧账本（对账输入 B）。
//
// 代理侧接口的字段名尚未与我方联调确认（契约 §7 已记为风险），
// 因此这里把所有「字段名假设」收敛到 pick* 系列函数：字段缺失/改名时降级为 0 而不是整窗报错。
// 只有 HTTP 层失败（连不上 / 非 2xx）才返回 error。
package usagebilling

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LedgerPath 是代理侧用量查询端点（相对 base_url）。
const LedgerPath = "/v1/magpie/usage/requests"

// ledgerBatchSize 是单次请求的 callerKey 上限（避免 URL 过长）。
const ledgerBatchSize = 50

// LedgerHTTP 是 LedgerClient 的 HTTP 实现。
type LedgerHTTP struct {
	baseURL    string
	adminKey   string
	authHeader string
	client     *http.Client
}

// NewLedgerHTTP 装配账本拉取器。authHeader 缺省 Authorization，值形如 `Bearer <admin_key>`。
func NewLedgerHTTP(baseURL, adminKey, authHeader string, client *http.Client) *LedgerHTTP {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if authHeader == "" {
		authHeader = "Authorization"
	}
	return &LedgerHTTP{
		baseURL:    strings.TrimRight(baseURL, "/"),
		adminKey:   adminKey,
		authHeader: authHeader,
		client:     client,
	}
}

// FetchWindow 分批拉取 [from,to) 内这些 callerKey 的用量并聚合成分组。
func (l *LedgerHTTP) FetchWindow(ctx context.Context, from, to time.Time, callerKeys []string) ([]Group, error) {
	var out []Group
	for start := 0; start < len(callerKeys); start += ledgerBatchSize {
		end := start + ledgerBatchSize
		if end > len(callerKeys) {
			end = len(callerKeys)
		}
		items, err := l.fetchBatch(ctx, from, to, callerKeys[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// fetchBatch 拉一批 key。
func (l *LedgerHTTP) fetchBatch(ctx context.Context, from, to time.Time, keys []string) ([]Group, error) {
	q := url.Values{}
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	q.Set("callerKey", strings.Join(keys, ","))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+LedgerPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if l.adminKey != "" {
		v := l.adminKey
		if !strings.Contains(v, " ") {
			v = "Bearer " + v
		}
		req.Header.Set(l.authHeader, v)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("代理用量接口返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseLedgerBody(body)
}

// parseLedgerBody 兼容三种外层形状：{items:[]} / {data:[]} / 顶层数组。
func parseLedgerBody(body []byte) ([]Group, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err == nil {
		for _, key := range []string{"items", "data", "requests", "list"} {
			if raw, ok := envelope[key]; ok {
				var items []map[string]any
				if err := json.Unmarshal(raw, &items); err == nil {
					return groupsFromItems(items), nil
				}
			}
		}
		return nil, fmt.Errorf("代理用量接口缺少 items/data 数组")
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err == nil {
		return groupsFromItems(items), nil
	}
	return nil, fmt.Errorf("代理用量接口响应无法解析")
}

// groupsFromItems 把条目聚合成分组（同一 callerKey+model 可能有多条）。
func groupsFromItems(items []map[string]any) []Group {
	agg := map[string]Group{}
	for _, it := range items {
		key := pickString(it, "caller_key_name", "callerKey", "caller_key", "key_name", "key")
		model := pickString(it, "model", "magpie_model", "internal_model")
		g := Group{
			CallerKey: key,
			Model:     model,
			Requests:  pickInt(it, "requests", "count", "req_count", "request_count"),
			Input:     pickInt(it, "input_tokens", "prompt_tokens", "inputTokens"),
			CacheRead: pickInt(it, "cache_read_tokens", "cached_tokens", "cache_read_input_tokens"),
			Output:    pickInt(it, "output_tokens", "completion_tokens", "outputTokens"),
		}
		if g.Requests == 0 {
			g.Requests = 1 // 逐条返回时每条即一笔
		}
		// 兼容 tokens 子对象 {tokens:{input,cache_read,output}}。
		if nested, ok := it["tokens"].(map[string]any); ok {
			if v := pickInt(nested, "input", "input_tokens"); v != 0 {
				g.Input = v
			}
			if v := pickInt(nested, "cache_read", "cache_read_tokens"); v != 0 {
				g.CacheRead = v
			}
			if v := pickInt(nested, "output", "output_tokens"); v != 0 {
				g.Output = v
			}
		}
		k := g.groupKey()
		cur := agg[k]
		cur.CallerKey, cur.Model = g.CallerKey, g.Model
		cur.Requests += g.Requests
		cur.Input += g.Input
		cur.CacheRead += g.CacheRead
		cur.Output += g.Output
		agg[k] = cur
	}
	out := make([]Group, 0, len(agg))
	for _, g := range agg {
		out = append(out, g)
	}
	return out
}

// pickString 取第一个存在且为字符串的候选字段。
func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// pickInt 取第一个存在且可转整数的候选字段（数字或数字字符串都接受）。
func pickInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i
			}
		case string:
			if i, err := strconv.ParseInt(n, 10, 64); err == nil {
				return i
			}
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
