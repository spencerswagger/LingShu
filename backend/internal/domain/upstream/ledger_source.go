package upstream

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

// LedgerPath 是上游（代理侧）用量查询端点（账本链路端点 UPSTREAM_LEDGER_URL 下的路径）。
//
// 契约来源：代理侧已提供的 `GET /v1/magpie/usage/requests`（同 `usagebilling` 分支的
// `LedgerPath`）。本文件的字段名假设与该分支收敛到同一处，不另立一套。
const LedgerPath = "/v1/magpie/usage/requests"

// httpLedgerMaxPage 是单页上限（与代理侧约定一致，避免一次拉爆）。
const httpLedgerMaxPage = 1000

// HTTPLedgerSource 是 PageSource 的 HTTP 实现：从上游用量接口按窗口 + offset 翻页。
//
// **它不做本地排序**（重要）：F9 的能力探测要看的正是「上游给的顺序」。
// 在这里补一个本地 sort 会把「上游乱序」这件事完全掩盖掉，探测就永远返回「支持」。
type HTTPLedgerSource struct {
	baseURL    string
	adminKey   string
	authHeader string
	client     *http.Client
}

// NewHTTPLedgerSource 装配 HTTP 分页源。authHeader 缺省 Authorization；adminKey 会自动补 Bearer。
func NewHTTPLedgerSource(baseURL, adminKey, authHeader string, client *http.Client) *HTTPLedgerSource {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if authHeader == "" {
		authHeader = "Authorization"
	}
	return &HTTPLedgerSource{
		baseURL:    strings.TrimRight(baseURL, "/"),
		adminKey:   adminKey,
		authHeader: authHeader,
		client:     client,
	}
}

// FetchPage 拉取 [w.Start, w.End) 内的一页。HasMore 按「本页取满」判定
// （上游若回 `next_cursor` 也视为还有更多，但本切片只发 offset，不臆造 cursor 参数）。
func (s *HTTPLedgerSource) FetchPage(ctx context.Context, w Window, offset, limit int) (Page, error) {
	if s == nil || s.client == nil || s.baseURL == "" {
		return Page{}, fmt.Errorf("上游账本分页源未配置基址")
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > httpLedgerMaxPage {
		limit = httpLedgerMaxPage
	}

	q := url.Values{}
	q.Set("from", w.Start.UTC().Format(time.RFC3339))
	q.Set("to", w.End.UTC().Format(time.RFC3339))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+LedgerPath+"?"+q.Encode(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("构建上游账本请求失败：%w", err)
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
		return Page{}, fmt.Errorf("请求上游账本失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Page{}, fmt.Errorf("读取上游账本响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Page{}, fmt.Errorf("上游账本接口返回 %d：%s", resp.StatusCode, truncateForLog(string(body)))
	}

	items, nextCursor, err := parseLedgerItems(body)
	if err != nil {
		return Page{}, err
	}
	entries := make([]LedgerEntry, 0, len(items))
	for _, it := range items {
		entries = append(entries, ledgerEntryFromItem(it))
	}
	hasMore := len(entries) >= limit || nextCursor != ""
	return Page{Entries: entries, HasMore: hasMore}, nil
}

// parseLedgerItems 兼容三种外层形状（{items:[]} / {data:[]} / 顶层数组），并回传 next_cursor（若有）。
func parseLedgerItems(body []byte) ([]map[string]any, string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err == nil {
		cursor := ""
		if raw, ok := envelope["next_cursor"]; ok {
			_ = json.Unmarshal(raw, &cursor)
		}
		for _, key := range []string{"items", "data", "requests", "list"} {
			if raw, ok := envelope[key]; ok {
				var items []map[string]any
				if err := json.Unmarshal(raw, &items); err == nil {
					return items, cursor, nil
				}
			}
		}
		return nil, cursor, fmt.Errorf("上游账本响应缺少 items/data 数组")
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err == nil {
		return items, "", nil
	}
	return nil, "", fmt.Errorf("上游账本响应无法解析")
}

// ledgerEntryFromItem 把一条上游条目映射成 LedgerEntry（字段名假设与 usagebilling 分支同源）。
//
// OccurredAt 解析失败时返回零值时间——探测会把它当成「回退」而判「不支持」，
// 这是有意的：时间戳都读不出来，就没有任何依据说上游的排序是可信的。
func ledgerEntryFromItem(it map[string]any) LedgerEntry {
	e := LedgerEntry{
		LedgerID:        pickStringField(it, "id", "request_id", "requestId", "record_id", "rid", "trace_id"),
		Provider:        pickStringField(it, "provider", "upstream_provider"),
		ProviderAccount: pickStringField(it, "account", "provider_account", "caller_key_name", "callerKey"),
		Model:           pickStringField(it, "model", "magpie_model", "internal_model"),
	}
	if v, ok := pickIntField(it, "input_tokens", "prompt_tokens", "inputTokens"); ok {
		e.TokensIn = v
	}
	if v, ok := pickIntField(it, "output_tokens", "completion_tokens", "outputTokens"); ok {
		e.TokensOut = v
	}
	if v, ok := pickFloatField(it, "cost_credits", "cost", "credits"); ok {
		e.CostCredits = v
	}
	if ts := pickStringField(it, "occurred_at", "created_at", "timestamp", "time", "ts", "started_at"); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.OccurredAt = t
		} else if t, err := time.Parse(time.RFC3339, ts); err == nil {
			e.OccurredAt = t
		}
	}
	return e
}

// pickStringField 返回第一个存在且非空的字符串字段（字段名改名时的兼容面）。
func pickStringField(it map[string]any, names ...string) string {
	for _, n := range names {
		switch v := it[n].(type) {
		case string:
			if v != "" {
				return v
			}
		case json.Number:
			return v.String()
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}

// pickIntField 返回第一个可解析的整数字段。
func pickIntField(it map[string]any, names ...string) (int64, bool) {
	for _, n := range names {
		switch v := it[n].(type) {
		case float64:
			return int64(v), true
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return i, true
			}
		case string:
			if i, err := strconv.ParseInt(v, 10, 64); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

// pickFloatField 返回第一个可解析的浮点字段。
func pickFloatField(it map[string]any, names ...string) (float64, bool) {
	for _, n := range names {
		switch v := it[n].(type) {
		case float64:
			return v, true
		case json.Number:
			if f, err := v.Float64(); err == nil {
				return f, true
			}
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// truncateForLog 截断错误响应体，避免把上游长报文灌进日志。
func truncateForLog(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
