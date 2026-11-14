// ledger.go 拉取代理侧账本（对账输入 B）。
//
// 代理侧接口的字段名尚未与我方联调确认（契约 §7 已记为风险），
// 因此这里把所有「字段名假设」收敛到 pick* 系列函数：字段缺失/改名时降级为 0 而不是整窗报错。
// 只有 HTTP 层失败（连不上 / 非 2xx）才返回 error。
//
// 契约 §8.2「最小字段集 + 降级表」在这里落地：每次响应都把 9 项的可见性记进 LedgerReport。
//   - 必需项（逐笔键、带时区时间戳、caller key、model、in/out tokens、failed、retried）缺任一，
//     或分页只支持 offset 形式 → 该窗口**不可对账**：标 `blocked: pending POC`
//     （既不算通过、也不算不通过，未做 POC 就不许写通过）；
//   - cache_read 可降级，但必须在 detail 里显式声明缺失（不猜、不补 0 当"真值"用）。
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

// ledgerPageSize 是单页条数上限（翻页只按 offset 发；cursor 只有接口回了字段才认）。
const ledgerPageSize = 1000

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

// BlockedPendingPOC 是「端到端未验（未做 POC）」的固定标记（契约 §8.2）：
// 标它既不算通过、也不算不通过——不许把未验当通过写进窗口结果。
const BlockedPendingPOC = "blocked: pending POC"

// 最小字段集的项名（与 LedgerReport.Missing 一致，Gate 断言直接用这些名字）。
const (
	FieldRecordKey = "record_key"
	FieldTimestamp = "timestamp"
	FieldCallerKey = "caller_key"
	FieldModel     = "model"
	FieldInput     = "input_tokens"
	FieldOutput    = "output_tokens"
	FieldFailed    = "failed"
	FieldRetried   = "retried"
	FieldCacheRead = "cache_read"
)

// RequiredLedgerFields 是契约 §8.2 的必需项（缺任一 → 该窗口不可对账）。
func RequiredLedgerFields() []string {
	return []string{FieldRecordKey, FieldTimestamp, FieldCallerKey, FieldModel,
		FieldInput, FieldOutput, FieldFailed, FieldRetried}
}

// LedgerReport 是一次账本响应的字段可见性与分页能力（降级表的输入，随窗口落进 detail）。
type LedgerReport struct {
	// Items 是接口返回的条目数；Records 是其中可解析的逐笔记录数。
	Items   int `json:"items"`
	Records int `json:"records"`
	// Missing 是必需项里「一条都没看到」的项名（非空 → 不可对账）。
	Missing []string `json:"missing,omitempty"`
	// CacheReadMissing 显式声明缓存命中的 token 缺失（可降级，但必须标记）。
	CacheReadMissing bool `json:"cache_read_missing"`
	// Paging 是接口支持的翻页形态：cursor / offset / unknown（"none" = 单页返回全部）。
	Paging string `json:"paging"`
	// FailedRecords / RetriedRecords 是被排除出 token 对比的逐笔数（failed 单不该参与对账）。
	FailedRecords  int `json:"failed_records"`
	RetriedRecords int `json:"retried_records"`
	// TimestampWithoutZone 表示有时间戳字段但**不带时区**（等价于缺时区 → 不可对账）。
	TimestampWithoutZone bool `json:"timestamp_without_zone"`
}

// BlockedReasons 返回不可对账的原因（空 = 可以做本地比对）。
func (r LedgerReport) BlockedReasons() []string {
	var out []string
	if len(r.Missing) > 0 {
		if r.Records == 0 {
			out = append(out, "no_samples")
		}
		out = append(out, "missing_fields:"+strings.Join(r.Missing, ","))
	}
	if r.TimestampWithoutZone {
		out = append(out, "timestamp_without_timezone")
	}
	if r.Paging != "cursor" && r.Paging != "none" {
		// 只支持 offset 形式（或压根看不出来）都不许当成已验证：不臆造 cursor 支持。
		out = append(out, "paging:"+r.Paging)
	}
	return out
}

// Reconcilable 报告本次响应是否具备最小字段集（可以进主口径比对）。
func (r LedgerReport) Reconcilable() bool { return len(r.BlockedReasons()) == 0 }

// FetchWindow 分批拉取 [from,to) 内这些 callerKey 的用量并聚合成分组。
//
// 第二个返回值是字段可见性报告：多批之间取「任一批见到即算见到」，
// 但**一批记录都没有**时视为未验（无法确认字段集）→ 同样不可对账（由调用方标 blocked）。
func (l *LedgerHTTP) FetchWindow(ctx context.Context, from, to time.Time, callerKeys []string) ([]Group, LedgerReport, error) {
	var out []Group
	acc := newLedgerReportAcc()
	for start := 0; start < len(callerKeys); start += ledgerBatchSize {
		end := start + ledgerBatchSize
		if end > len(callerKeys) {
			end = len(callerKeys)
		}
		items, rep, err := l.fetchBatch(ctx, from, to, callerKeys[start:end])
		if err != nil {
			return nil, acc.finish(), err
		}
		out = append(out, items...)
		acc.add(rep)
	}
	return out, acc.finish(), nil
}

// ledgerReportAcc 跨批累积字段可见性（口径：任一批的记录里见到就算见到）。
type ledgerReportAcc struct {
	items       int
	records     int
	failed      int
	retried     int
	paging      string
	cacheRead   bool
	tzMissing   bool
	saw         map[string]bool
	batchesSeen int
}

func newLedgerReportAcc() *ledgerReportAcc {
	return &ledgerReportAcc{saw: map[string]bool{}, paging: "unknown"}
}

// add 吸收一批的报告（只统计有记录的批次：空批次不能证明也不能否定字段集）。
func (a *ledgerReportAcc) add(rep LedgerReport) {
	a.items += rep.Items
	a.records += rep.Records
	a.failed += rep.FailedRecords
	a.retried += rep.RetriedRecords
	a.tzMissing = a.tzMissing || rep.TimestampWithoutZone
	if (a.paging == "unknown" || a.paging == "") && rep.Paging != "" {
		a.paging = rep.Paging
	}
	if rep.Records == 0 {
		return
	}
	a.batchesSeen++
	if !rep.CacheReadMissing {
		a.cacheRead = true
	}
	missing := map[string]bool{}
	for _, f := range rep.Missing {
		missing[f] = true
	}
	for _, f := range RequiredLedgerFields() {
		if !missing[f] {
			a.saw[f] = true
		}
	}
}

// finish 产出最终报告：必需项里「一条都没见到」的即缺。
func (a *ledgerReportAcc) finish() LedgerReport {
	rep := LedgerReport{Items: a.items, Records: a.records, FailedRecords: a.failed,
		RetriedRecords: a.retried, TimestampWithoutZone: a.tzMissing, Paging: a.paging,
		CacheReadMissing: !a.cacheRead && a.batchesSeen > 0}
	if a.batchesSeen == 0 {
		// 一条记录都没有：字段集无从确认（未验），按不可对账处理并显式记 no_samples。
		rep.Missing = RequiredLedgerFields()
		rep.CacheReadMissing = true
		return rep
	}
	for _, f := range RequiredLedgerFields() {
		if !a.saw[f] {
			rep.Missing = append(rep.Missing, f)
		}
	}
	return rep
}

// fetchBatch 拉一批 key。
func (l *LedgerHTTP) fetchBatch(ctx context.Context, from, to time.Time, keys []string) ([]Group, LedgerReport, error) {
	q := url.Values{}
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	q.Set("callerKey", strings.Join(keys, ","))
	// 只发 offset 形式（不臆造 cursor 参数）；接口若回 next_cursor 才认它支持 cursor。
	q.Set("limit", strconv.Itoa(ledgerPageSize))
	q.Set("offset", "0")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.baseURL+LedgerPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, LedgerReport{Paging: "unknown"}, err
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
		return nil, LedgerReport{Paging: "unknown"}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, LedgerReport{Paging: "unknown"}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, LedgerReport{Paging: "unknown"},
			fmt.Errorf("代理用量接口返回 %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseLedgerBody(body)
}

// parseLedgerBody 兼容三种外层形状：{items:[]} / {data:[]} / 顶层数组；
// 同时把「最小字段集可见性」记进 LedgerReport（缺字段不再静默当 0）。
func parseLedgerBody(body []byte) ([]Group, LedgerReport, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err == nil {
		for _, key := range []string{"items", "data", "requests", "list"} {
			if raw, ok := envelope[key]; ok {
				var items []map[string]any
				if err := json.Unmarshal(raw, &items); err == nil {
					return groupsFromItems(items, envelope)
				}
			}
		}
		return nil, LedgerReport{Paging: "unknown"}, fmt.Errorf("代理用量接口缺少 items/data 数组")
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err == nil {
		return groupsFromItems(items, nil)
	}
	return nil, LedgerReport{Paging: "unknown"}, fmt.Errorf("代理用量接口响应无法解析")
}

// groupsFromItems 把逐笔条目聚合成分组，并把字段可见性写进报告。
//
// 聚合口径：failed 单不参与 token 对比（灵枢侧也只统计 completed），但笔数与类别记进报告，
// 让 detail 能解释「为什么笔数不一致」。
func groupsFromItems(items []map[string]any, envelope map[string]json.RawMessage) ([]Group, LedgerReport, error) {
	rep := LedgerReport{Items: len(items), Paging: detectPaging(envelope)}
	agg := map[string]Group{}
	var sawRecordKey, sawTimestamp, sawCallerKey, sawModel, sawInput, sawOutput bool
	var sawFailed, sawRetried, sawCacheRead bool
	for _, it := range items {
		key := pickString(it, "caller_key_name", "callerKey", "caller_key", "key_name", "key")
		model := pickString(it, "model", "magpie_model", "internal_model")
		recordKey := pickString(it, "id", "request_id", "requestId", "record_id", "rid", "trace_id")
		tsRaw := pickString(it, "created_at", "timestamp", "time", "ts", "started_at")
		input, hasInput := pickIntOK(it, "input_tokens", "prompt_tokens", "inputTokens")
		output, hasOutput := pickIntOK(it, "output_tokens", "completion_tokens", "outputTokens")
		cacheRead, hasCache := pickIntOK(it, "cache_read_tokens", "cached_tokens", "cache_read_input_tokens")
		failed, hasFailed := pickBoolOK(it, "failed", "is_failed", "error")
		retried, hasRetried := pickRetried(it)

		// tokens 子对象（{tokens:{input,cache_read,output}}）也算命中。
		if nested, ok := it["tokens"].(map[string]any); ok {
			if v, ok := pickIntOK(nested, "input", "input_tokens"); ok {
				input, hasInput = v, true
			}
			if v, ok := pickIntOK(nested, "cache_read", "cache_read_tokens"); ok {
				cacheRead, hasCache = v, true
			}
			if v, ok := pickIntOK(nested, "output", "output_tokens"); ok {
				output, hasOutput = v, true
			}
		}

		if recordKey != "" {
			sawRecordKey = true
		}
		if tsRaw != "" {
			sawTimestamp = true
			if !hasTimezone(tsRaw) {
				rep.TimestampWithoutZone = true
			}
		}
		if key != "" {
			sawCallerKey = true
		}
		if model != "" {
			sawModel = true
		}
		sawInput = sawInput || hasInput
		sawOutput = sawOutput || hasOutput
		sawCacheRead = sawCacheRead || hasCache
		sawFailed = sawFailed || hasFailed
		sawRetried = sawRetried || hasRetried
		rep.Records++

		if failed {
			rep.FailedRecords++
			continue // 失败单不参与 token 对比
		}
		if retried {
			rep.RetriedRecords++
		}
		g := Group{CallerKey: key, Model: model, Input: input, CacheRead: cacheRead, Output: output}
		if !hasFailed && !hasRetried {
			// 逐笔字段缺省时按「一笔一条」的旧形状处理（仍会被报告标成不可对账）。
			g.Requests = 1
		} else {
			g.Requests = 1
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

	rep.CacheReadMissing = !sawCacheRead
	if !sawRecordKey {
		rep.Missing = append(rep.Missing, FieldRecordKey)
	}
	if !sawTimestamp {
		rep.Missing = append(rep.Missing, FieldTimestamp)
	}
	if !sawCallerKey {
		rep.Missing = append(rep.Missing, FieldCallerKey)
	}
	if !sawModel {
		rep.Missing = append(rep.Missing, FieldModel)
	}
	if !sawInput {
		rep.Missing = append(rep.Missing, FieldInput)
	}
	if !sawOutput {
		rep.Missing = append(rep.Missing, FieldOutput)
	}
	if !sawFailed {
		rep.Missing = append(rep.Missing, FieldFailed)
	}
	if !sawRetried {
		rep.Missing = append(rep.Missing, FieldRetried)
	}

	out := make([]Group, 0, len(agg))
	for _, g := range agg {
		out = append(out, g)
	}
	return out, rep, nil
}

// detectPaging 判定接口支持的翻页形态：有 cursor 字段才算 cursor；有 total/offset 就是 offset；
// 两者都没有且条目没到页大小 → none（单页拿全）；否则 unknown（看不出来，按不可对账处理）。
func detectPaging(envelope map[string]json.RawMessage) string {
	if envelope != nil {
		for _, k := range []string{"next_cursor", "nextCursor", "cursor", "next_page_token"} {
			if raw, ok := envelope[k]; ok {
				var s string
				_ = json.Unmarshal(raw, &s)
				return "cursor" // 显式回了 cursor 字段才认（不臆造支持）
			}
		}
		for _, k := range []string{"total", "total_count", "totalCount", "offset"} {
			if _, ok := envelope[k]; ok {
				return "offset"
			}
		}
	}
	return "offset" // 我方请求就是 offset 形式：接口没给 cursor 就等于只支持 offset
}

// hasTimezone 判定时间戳字符串是否带时区（`Z` 或 ±hh:mm / ±hhmm 后缀）。
func hasTimezone(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "z") {
		return true
	}
	// 末尾 ±hh:mm 或 ±hhmm
	if len(s) >= 6 {
		tail := s[len(s)-6:]
		if (tail[0] == '+' || tail[0] == '-') && tail[3] == ':' {
			return true
		}
	}
	if len(s) >= 5 {
		tail := s[len(s)-5:]
		if tail[0] == '+' || tail[0] == '-' {
			allDigit := true
			for _, c := range tail[1:] {
				if c < '0' || c > '9' {
					allDigit = false
					break
				}
			}
			if allDigit {
				return true
			}
		}
	}
	// RFC3339 解析成功也说明带时区。
	if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return true
	}
	return false
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

// pickIntOK 同 pickInt，但同时报告「字段是否存在」（缺字段 ≠ 值为 0）。
func pickIntOK(m map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, true
			}
		case string:
			if i, err := strconv.ParseInt(n, 10, 64); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

// pickBoolOK 取布尔字段（兼容 true/false、"true"、1/0）。
func pickBoolOK(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			switch strings.ToLower(b) {
			case "true", "failed", "error":
				return true, true
			case "false", "ok", "succeeded", "success":
				return false, true
			}
		case float64:
			return b != 0, true
		}
	}
	return false, false
}

// pickRetried 取「是否重试过」：retried 布尔或 retry_count>0 都算。
func pickRetried(m map[string]any) (bool, bool) {
	if b, ok := pickBoolOK(m, "retried", "is_retried"); ok {
		return b, true
	}
	if n, ok := pickIntOK(m, "retry_count", "retries"); ok {
		return n > 0, true
	}
	return false, false
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
