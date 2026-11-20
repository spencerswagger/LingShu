// magpie_ledger.go 是代理侧账本接口（magpie `GET /v1/magpie/usage/requests`）的适配层。
//
// 为什么单独一层：灵枢只读代理侧的账本，**不改代理侧**（spencer 已定「透传不改它」）。
// 代理侧的真实口径与最初按「通用分页接口」写的假设差了四处，全部在本文件收敛：
//
//   - 字段名：行数组是 `rows`（不是 items/data/requests/list）；时间 `t`；token `in`/`out`；
//     订阅账号 `providerAccount`（camelCase；`callerKeyName` 是网关调用方 key，不是账号）；
//     成本 `cost`（USD，`priced=false` 时无意义）；模型 `model`。
//   - token 口径：`in` 是**未缓存输入**，缓存读/写是独立字段 `cache_read`/`cache_write`
//     （代理侧 docs/reference.md：tokens counted are a call's uncached input, output and
//     cache writes, plus cache reads）。灵枢账单侧是「input（含 cache_write）+ cache_read + output」，
//     所以账本侧必须把缓存并进 `tokens_in`，否则两侧合计永远不等（见 mapMagpieFeedRow）。
//   - 排序：offset 账本是**最新在前**（newest first），不是 `occurred_at ASC`。
//     cursor feed 是「按 seq 的变更流」，本身不承诺业务时间序。
//   - 幂等键：`id`（u_…）是代理侧自己的行 id，跨读取/重启不变；`original_ids.request_id`
//     只是来源给的 id，**不是**幂等键，代理侧也从不编造。同一 id 以更高 `seq` 再次出现 =
//     这一行被改过，要覆盖而不是忽略。
//   - 时间过滤：offset 模式只认 `period`（today/week/month/all 或 `from..to` 日期）与 `day`
//     （代理侧时区的 YYYY-MM-DD），**没有 from/to 时刻参数**。所以本适配层不用窗口时刻去过滤
//     上游，而是用官方的 cursor 变更流（`cursor=start|now|<next_cursor>`），窗口只作为灵枢
//     自己的对账/水位线口径。
//
// 权威来源：yetone/magpie `docs/reference.md` 与 `internal/usageapi/usageapi.go` /
// `usageapi_feed.go` / `ledger.go` 的 FeedRow/FeedResult/Gone 定义（2026-11-17 核对）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/decimalx"
)

const (
	// MagpieCursorStart 是代理侧 cursor feed 的「从头读」游标。
	MagpieCursorStart = "start"
	// MagpieMaxLimit 是代理侧 cursor/offset 接口的 limit 硬上限（超过会被 400）。
	MagpieMaxLimit = 500
	// magpieFeedReadBytes 是单页响应读取上限（正常一页 500 行，远小于此）。
	magpieFeedReadBytes = 16 << 20
)

// CostConverter 把代理侧账本的美元成本折算成灵枢积分。
//
// 代理侧 `cost` 是 USD；灵枢 `ledger_entries.cost_credits` 是积分。两者不是同一单位，
// 必须由装配层注入换算（公式与订阅录入同源：积分 = 元 × 1e6 ÷ R，USD 先 × cny_rate）。
// 未注入时按 **原值** 写入，只用于「账本留痕/排障」，不得直接当积分做盈亏（见 PR 说明）。
type CostConverter func(usd float64) (credits float64, err error)

// FeedPage 是代理侧 cursor feed 的一页。
type FeedPage struct {
	// Entries 是要 upsert 的账本行（按 id 幂等；同 id 高 seq 覆盖）。
	Entries []LedgerEntry
	// Deleted 是代理侧明确要求删除的账本 id（行被合并/清除）。
	Deleted []string
	// NextCursor 是下一轮的游标；HasMore 为 true 时必须有值。
	NextCursor string
	HasMore    bool
	// Skipped 是本页因缺幂等键/时间或 handed_on 而跳过的行数（可观测，不静默）。
	Skipped int
	// StatusCode 是本页对应上游 HTTP 状态（正常 2xx；伪源/内存源可留 0）。
	// 用途：连续空轮告警要能给出「最近一次 HTTP 状态」——200 空页 vs 连接层面的问题能区分开。
	StatusCode int
}

// LedgerFeed 是「按游标读变更」的账本源（代理侧 cursor feed 的实现）。
type LedgerFeed interface {
	FetchFeed(ctx context.Context, cursor string, limit int) (FeedPage, error)
}

// MagpieCursorError 是代理侧游标失效/非法错误。
//
// 语义（docs/reference.md）：
//   - 410 `cursor_expired` / `cursor_reset`：游标太旧或账本被重置，必须从 `start` 重读；
//   - 400 `cursor_invalid` / `cursor_filter_changed`：游标格式不对或过滤条件变了，同样从 `start` 重读。
//
// 重读是幂等的（按 id upsert + deleted 列表），所以这里不尝试「猜一个中间位置」。
type MagpieCursorError struct {
	StatusCode int
	Code       string
	Body       string
}

func (e *MagpieCursorError) Error() string {
	if e == nil {
		return "代理侧账本游标失效"
	}
	code := e.Code
	if code == "" {
		code = "unknown"
	}
	return fmt.Sprintf("代理侧账本游标失效（HTTP %d，code=%s）：%s", e.StatusCode, code, truncateForLog(e.Body))
}

// Reset 表示「必须从 start 重读」。
func (e *MagpieCursorError) Reset() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case "cursor_expired", "cursor_reset", "cursor_invalid", "cursor_filter_changed":
		return true
	}
	return e.StatusCode == http.StatusGone
}

// MagpieLedgerSource 是 LedgerFeed 的 HTTP 实现（cursor 变更流）。
type MagpieLedgerSource struct {
	baseURL    string
	adminKey   string
	authHeader string
	client     *http.Client
	convert    CostConverter
}

// NewMagpieLedgerSource 装配代理侧账本源。authHeader 缺省 Authorization；adminKey 自动补 Bearer。
func NewMagpieLedgerSource(baseURL, adminKey, authHeader string, client *http.Client, convert CostConverter) *MagpieLedgerSource {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if authHeader == "" {
		authHeader = "Authorization"
	}
	return &MagpieLedgerSource{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		adminKey:   strings.TrimSpace(adminKey),
		authHeader: authHeader,
		client:     client,
		convert:    convert,
	}
}

var _ LedgerFeed = (*MagpieLedgerSource)(nil)

// FetchFeed 读取一页变更。cursor 为空时等价 `start`。
func (s *MagpieLedgerSource) FetchFeed(ctx context.Context, cursor string, limit int) (FeedPage, error) {
	if s == nil || s.client == nil || s.baseURL == "" {
		return FeedPage{}, fmt.Errorf("代理侧账本源未配置基址")
	}
	if strings.TrimSpace(cursor) == "" {
		cursor = MagpieCursorStart
	}
	if limit <= 0 || limit > MagpieMaxLimit {
		limit = MagpieMaxLimit
	}

	q := url.Values{}
	q.Set("cursor", cursor)
	q.Set("limit", strconv.Itoa(limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+LedgerPath+"?"+q.Encode(), nil)
	if err != nil {
		return FeedPage{}, fmt.Errorf("构建代理侧账本请求失败：%w", err)
	}
	if s.adminKey != "" {
		v := s.adminKey
		if !strings.Contains(v, " ") {
			v = "Bearer " + v
		}
		req.Header.Set(s.authHeader, v)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return FeedPage{}, fmt.Errorf("请求代理侧账本失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, magpieFeedReadBytes))
	if err != nil {
		return FeedPage{}, fmt.Errorf("读取代理侧账本响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ce := &MagpieCursorError{
			StatusCode: resp.StatusCode,
			Code:       magpieErrorCode(body),
			Body:       string(body),
		}
		if ce.Reset() {
			return FeedPage{}, ce
		}
		return FeedPage{}, fmt.Errorf("代理侧账本接口返回 %d：%s", resp.StatusCode, truncateForLog(string(body)))
	}

	page, err := parseMagpieFeed(body, s.convert)
	if err != nil {
		return FeedPage{}, err
	}
	page.StatusCode = resp.StatusCode
	return page, nil
}

// magpieErrorCode 兼容 `{"code":"cursor_reset"}` / `{"error":"cursor_reset"}` /
// `{"error":{"code":"cursor_reset"}}` 三种错误体形状（代理侧不同版本用过不同形状）。
func magpieErrorCode(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	for _, key := range []string{"code", "error_code"} {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	switch v := m["error"].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["code"].(string); ok {
			return s
		}
		if s, ok := v["type"].(string); ok {
			return s
		}
	}
	return ""
}

// magpieFeedPayload 是代理侧 cursor feed 的真实响应形状。
type magpieFeedPayload struct {
	Rows       []magpieFeedRow     `json:"rows"`
	Deleted    []magpieFeedDeleted `json:"deleted"`
	NextCursor string              `json:"next_cursor"`
	HasMore    bool                `json:"has_more"`
}

type magpieFeedDeleted struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
}

// magpieFeedRow 只声明灵枢用得到的字段；未列出的字段（usage/分档/字节数等）忽略。
//
// 注意 json tag 与代理侧逐字一致：`providerAccount` 是 camelCase，`in`/`out`/`t` 都是短名。
type magpieFeedRow struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	Origin string `json:"origin"`
	// OriginalIDs 只作来源审计，**不是**幂等键（代理侧文档明确：id 才是，且从不编造来源 id）。
	OriginalIDs struct {
		RequestID        string `json:"request_id"`
		ResponseID       string `json:"response_id"`
		ClientResponseID string `json:"client_response_id"`
		MessageID        string `json:"message_id"`
		SessionID        string `json:"session_id"`
		NativeSessionID  string `json:"native_session_id"`
	} `json:"original_ids"`
	T               string `json:"t"`
	Provider        string `json:"provider"`
	ProviderAccount string `json:"providerAccount"`
	ProviderKeyName string `json:"providerKeyName"`
	CallerKeyName   string `json:"callerKeyName"`
	Model           string `json:"model"`
	Req             string `json:"req"`
	Served          string `json:"served"`
	In              int64  `json:"in"`
	Out             int64  `json:"out"`
	CacheRead       int64  `json:"cache_read"`
	CacheWrite      int64  `json:"cache_write"`
	CacheWrite1h    int64  `json:"cache_write_1h"`
	Reasoning       int64  `json:"reasoning"`
	Status          int    `json:"status"`
	Err             string `json:"err"`
	ErrType         string `json:"err_type"`
	HandedOn        bool   `json:"handed_on"`
	Rejected        bool   `json:"rejected"`
	// RouteID 是网关为本请求分配的路由 id（同一路由的多次 try 共享）；session/reported 行没有 → 0。
	RouteID int64   `json:"route_id"`
	Cost    float64 `json:"cost"`
	Priced  bool    `json:"priced"`
}

// parseMagpieFeed 把代理侧真实报文映射成 FeedPage。
func parseMagpieFeed(body []byte, convert CostConverter) (FeedPage, error) {
	// 先做形状探测：只认 cursor feed 的 rows。旧实现认 items/data 会在真实报文上静默返回 0 行，
	// 那种「不报错的空轮」正是 F9 要防的假绿，所以这里显式报错。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return FeedPage{}, fmt.Errorf("代理侧账本响应不是 JSON 对象：%w", err)
	}
	if _, ok := probe["rows"]; !ok {
		return FeedPage{}, fmt.Errorf("代理侧账本响应缺少 rows 数组（真实口径是 rows，不是 items/data/requests/list）")
	}

	var p magpieFeedPayload
	d := json.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(&p); err != nil {
		return FeedPage{}, fmt.Errorf("解析代理侧账本响应失败：%w", err)
	}

	page := FeedPage{NextCursor: strings.TrimSpace(p.NextCursor), HasMore: p.HasMore}
	for _, it := range p.Deleted {
		if id := strings.TrimSpace(it.ID); id != "" {
			page.Deleted = append(page.Deleted, id)
		}
	}
	for _, row := range p.Rows {
		entry, ok, err := mapMagpieFeedRow(row, convert)
		if err != nil {
			return FeedPage{}, err
		}
		if !ok {
			page.Skipped++
			continue
		}
		page.Entries = append(page.Entries, entry)
	}
	return page, nil
}

// mapMagpieFeedRow 把一行映射成灵枢账本九字段；ok=false 表示该行按口径跳过。
//
// 跳过规则（都写进 FeedPage.Skipped，不静默）：
//   - 没有 `id`：没有幂等键，写进去迟早重复计账；
//   - 没有/无法解析 `t`：occurred_at 是 NOT NULL 语义，不许 now() 兜底；
//   - `handed_on=true`：代理侧文档明确「既不算请求也不算失败」，写进灵枢会把笔数算重。
func mapMagpieFeedRow(row magpieFeedRow, convert CostConverter) (LedgerEntry, bool, error) {
	id := strings.TrimSpace(row.ID)
	if id == "" || row.HandedOn {
		return LedgerEntry{}, false, nil
	}
	occurred, err := parseMagpieTime(row.T)
	if err != nil {
		return LedgerEntry{}, false, nil
	}
	if row.In < 0 {
		row.In = 0
	}
	if row.Out < 0 {
		row.Out = 0
	}
	if row.CacheRead < 0 {
		row.CacheRead = 0
	}
	if row.CacheWrite < 0 {
		row.CacheWrite = 0
	}

	cost := 0.0
	if row.Priced {
		cost = row.Cost
		if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
			cost = 0
		}
		if cost != 0 && convert == nil {
			// 不能把 USD 直接写进「积分」列：宁整页报错（窗口 blocked），也不静默错单位。
			return LedgerEntry{}, false, fmt.Errorf("账本成本未配置换算（id=%s, cost=%.6f USD）", id, cost)
		}
		if convert != nil && cost != 0 {
			converted, cerr := convert(cost)
			if cerr != nil {
				return LedgerEntry{}, false, fmt.Errorf("换算账本成本失败（id=%s, cost=%.6f USD）：%w", id, cost, cerr)
			}
			cost = converted
		}
	}

	model := strings.TrimSpace(row.Model)
	if model == "" {
		// 代理侧的 served 是上游回包自报的模型 id；仅在 model 缺失时兜底（不覆盖正常值）。
		model = strings.TrimSpace(row.Served)
	}
	// tokens_in 存**提示词侧总量** = in + cache_read + cache_write。
	//
	// 为什么合并而不是原样搬 `in`：灵枢 billing_records 的三段口径是「纯输入（含 cache_write）
	// + cache_read + output」，对账/结算都按 `billing.TokenSumExpr` 求和。代理侧的 `in`
	// **不含**缓存（缓存读/写是独立字段），原样搬会让账本侧恒小于本地（真库实测 10200 vs 20440），
	// 对账每窗误判 alarm。合并后两侧合计同口径，且账本表仍是九字段（不新增缓存列）。
	// `cache_write_1h` 是 `cache_write` 按 TTL 的**拆分**（子集），不能重复相加；
	// `reasoning` 已经包含在 `out` 里，也不再另加。
	promptTokens := row.In + row.CacheRead + row.CacheWrite
	// route_id：网关为本请求分配的路由 id；负值是脏数据，归 0（0 = 无/未知，结算侧走回退链）。
	if row.RouteID < 0 {
		row.RouteID = 0
	}
	return LedgerEntry{
		LedgerID:        id,
		Provider:        strings.TrimSpace(row.Provider),
		ProviderAccount: strings.TrimSpace(row.ProviderAccount),
		Model:           model,
		TokensIn:        promptTokens,
		TokensOut:       row.Out,
		CostCredits:     decimalx.Round5(cost),
		OccurredAt:      occurred,
		// 五个细分维度原样搬运（缺 caller_key_name 会让 poller settleOne 无法归属令牌，账单出不来）：
		ReqModel:      strings.TrimSpace(row.Req),
		ServedModel:   strings.TrimSpace(row.Served),
		CallerKeyName: strings.TrimSpace(row.CallerKeyName),
		CacheRead:     row.CacheRead,
		CacheWrite:    row.CacheWrite,
		RouteID:       row.RouteID,
	}, true, nil
}

// parseMagpieTime 解析代理侧 `t`（RFC3339，可带纳秒/时区偏移）。
func parseMagpieTime(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("缺少时间 t")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("解析时间 t=%q 失败：%w", s, err)
	}
	return t, nil
}
