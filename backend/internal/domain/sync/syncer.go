package sync

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Syncer 定时拉取外部价格源并与 watchlist 比对，产生价格变更告警。
// 全部可测试依赖通过字段注入（httptest fake source / 可注入 now）。
type Syncer struct {
	store          *Store
	client         *http.Client
	sourceURL      string
	interval       time.Duration
	now            func() time.Time
	logger         *slog.Logger
	pricesMu       sync.RWMutex
	lastPrices     map[string]Pricing       // 最近一次拉取的全量价格（内存缓存供查看）
	lastPricesFull map[string]ExternalPrice // 同快照，保留 provider 维度（价格目录搜索用）

	lastUpdateMu  sync.RWMutex
	lastUpdatedAt time.Time // 最近一次成功拉取时间（参考价 updated_at 用）
}

// SyncerConfig 是 NewSyncer 的入参。
type SyncerConfig struct {
	Store     *Store
	Client    *http.Client // nil 用 http.DefaultClient
	SourceURL string
	Interval  time.Duration // run-once 后按该间隔定时；<=0 时用 60min
	Now       func() time.Time
	Logger    *slog.Logger
}

// NewSyncer 创建同步器。
func NewSyncer(cfg SyncerConfig) *Syncer {
	if cfg.Client == nil {
		cfg.Client = http.DefaultClient
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Syncer{
		store:          cfg.Store,
		client:         cfg.Client,
		sourceURL:      cfg.SourceURL,
		interval:       cfg.Interval,
		now:            cfg.Now,
		logger:         cfg.Logger,
		lastPrices:     make(map[string]Pricing),
		lastPricesFull: make(map[string]ExternalPrice),
	}
}

// Run 启动同步循环：启动时执行一次，随后按 interval 定时执行。
// 单次拉取失败仅记日志不 panic（下一周期重试）。
func (s *Syncer) Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := s.SyncOnce(ctx); err != nil {
		s.logger.Error("sync once at startup failed", "err", err)
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.SyncOnce(ctx); err != nil {
				s.logger.Error("sync tick failed", "err", err)
			}
		}
	}
}

// SyncOnce 执行一次同步：拉取 → 缓存 → 遍历 watchlist 比对 → 产生告警。
// 返回 human 可读的摘要串。真实拉取失败返回 error（不 panic）。
func (s *Syncer) SyncOnce(ctx context.Context) (string, error) {
	prices, err := fetchSource(ctx, s.client, s.sourceURL)
	if err != nil {
		return "", err
	}
	pmap := make(map[string]ExternalPrice, len(prices))
	fresh := make(map[string]Pricing, len(prices))
	for _, p := range prices {
		// 复合 key「provider/modelID」防覆盖：同一模型在不同供应商下 ID 相同但价格不同
		pmap[ProviderModelKey(p.Provider, p.ID)] = p
		fresh[p.ID] = p.Pricing
	}
	s.pricesMu.Lock()
	s.lastPrices = fresh
	s.lastPricesFull = pmap
	s.pricesMu.Unlock()
	// 记录拉取时间（参考价接口 updated_at）。
	now := s.now()
	s.lastUpdateMu.Lock()
	s.lastUpdatedAt = now
	s.lastUpdateMu.Unlock()

	items, err := s.store.ListWatchlist(ctx)
	if err != nil {
		return "", fmt.Errorf("读取关注列表失败: %w", err)
	}

	checked := 0
	alertCount := 0
	for i := range items {
		item := items[i]
		price, ok := pmap[item.ExternalModelID]
		if !ok {
			continue // 价格源无此项 → 跳过不告警
		}
		checked++

		// 首次同步（无基线）：记录基线价格，不生成告警。
		if item.LastSyncedAt == nil {
			if err := s.store.SetBaseline(ctx, item.ID, price.Pricing.Prompt, price.Pricing.Completion, now); err != nil {
				return "", fmt.Errorf("记录基线失败: %w", err)
			}
			continue
		}

		changes := map[string]Change{}
		gather := func(k string, old *float64, new float64) {
			if old == nil {
				return // 基线缺失按首次同步处理
			}
			if c, changed := diffChange(*old, new); changed {
				changes[k] = c
			}
		}
		gather("input", item.PromptPrice, price.Pricing.Prompt)
		gather("output", item.CompletionPrice, price.Pricing.Completion)

		if len(changes) == 0 {
			if err := s.store.UpdateSyncedAt(ctx, item.ID, now); err != nil {
				return "", fmt.Errorf("刷新同步时间失败: %w", err)
			}
			continue
		}

		// 变化 → 若开启告警则插一条 pending 告警，随后更新基线到新价。
		if item.AlertOnChange {
			id := item.ID
			if _, err := s.store.InsertAlert(ctx, &PriceAlert{
				WatchlistItemID: &id,
				ExternalModelID: item.ExternalModelID,
				LocalModelName:  item.LocalModelName,
				Changes:         changes,
				Status:          AlertStatusPending,
				DetectedAt:      now,
			}); err != nil {
				return "", fmt.Errorf("写入告警失败: %w", err)
			}
			alertCount++
		}
		if err := s.store.SetBaseline(ctx, item.ID, price.Pricing.Prompt, price.Pricing.Completion, now); err != nil {
			return "", fmt.Errorf("更新基线失败: %w", err)
		}
	}

	return fmt.Sprintf("拉取 %d 个模型，检查 %d 个关注项，产生 %d 条告警",
		len(prices), checked, alertCount), nil
}

// diffChange 比较新旧值；两值相同返回 (zero, false)。change_pct=(new-old)/old*100。
func diffChange(old, new float64) (Change, bool) {
	if nearlyEqual(old, new) {
		return Change{}, false
	}
	pct := 0.0
	if old != 0 {
		pct = (new - old) / old * 100
	}
	return Change{Old: old, New: new, ChangePct: pct}, true
}

// nearlyEqual 浮点近似相等（1e-9 相对容差，绝对 1e-12 兜底）。
func nearlyEqual(a, b float64) bool {
	if math.Abs(a-b) <= 1e-12 {
		return true
	}
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}

// LastPrices 返回最近一次拉取的价格快照（内存缓存）。
func (s *Syncer) LastPrices() map[string]Pricing {
	s.pricesMu.RLock()
	defer s.pricesMu.RUnlock()
	out := make(map[string]Pricing, len(s.lastPrices))
	for k, v := range s.lastPrices {
		out[k] = v
	}
	return out
}

// GetPrice 按外部模型名（缓存 key，即 models.dev 模型 ID）查询最近一次拉取的参考价。
// 供对外模型「查看 models.dev 参考价 / 一键应用售价」接口使用；无缓存返回 (zero, false)。
func (s *Syncer) GetPrice(externalName string) (Pricing, bool) {
	s.pricesMu.RLock()
	defer s.pricesMu.RUnlock()
	p, ok := s.lastPrices[externalName]
	return p, ok
}

// ProviderModelKey 构造唯一复合键「provider/modelID」。
// models.dev 中部分转售商的模型 ID 本身就带 provider 前缀（如 openai/gpt-5.5），
// 因此必须始终携带本条目的 provider，否则跨供应商会互相覆盖。
// provider 为空（仅测试夹具会出现）时退化为原 ID。
func ProviderModelKey(provider, modelID string) string {
	if provider == "" {
		return modelID
	}
	return provider + "/" + modelID
}

// PriceEntry 价格目录条目：含供应商维度，供「搜索浏览 models.dev 参考价」使用。
// 价格均为美元 / 百万 token；cache_read / cache_write / reasoning 来自 models.dev cost 字段。
type PriceEntry struct {
	Provider      string  `json:"Provider"`
	ModelID       string  `json:"ModelID"`
	InputUSD      float64 `json:"InputUSD"`
	OutputUSD     float64 `json:"OutputUSD"`
	CacheUSD      float64 `json:"CacheReadUSD"`
	CacheWriteUSD float64 `json:"CacheWriteUSD"`
	ReasoningUSD  float64 `json:"ReasoningUSD"`
}

// Recommended 返回某外部模型名（即 models.dev 模型 ID）下最合适的参考价：
// 优先供应商名与模型名前缀一致的「官方/直连」条目（如 deepseek-flash → deepseek），
// 否则取按供应商名排序的第一条。未命中返回 (zero,false)。
func (s *Syncer) Recommended(externalName string) (ExternalPrice, bool) {
	s.pricesMu.RLock()
	defer s.pricesMu.RUnlock()
	prefix := externalName
	if i := strings.IndexByte(externalName, '-'); i > 0 {
		prefix = externalName[:i]
	}
	var best *ExternalPrice
	for _, p := range s.lastPricesFull {
		if p.ID != externalName {
			continue
		}
		if p.Provider == prefix {
			cur := p
			best = &cur
			break
		}
		if best == nil || p.Provider < best.Provider {
			cur := p
			best = &cur
		}
	}
	if best == nil {
		return ExternalPrice{}, false
	}
	return *best, true
}

// isOfficialEntry 判断某条目是否为「官方/直连」：供应商名与模型 ID 的 '/'(转售前缀)之前、
// '-'之前 的模型名前缀一致（如 deepseek / deepseek-flash）。转售商（openrouter、302ai…）不满足。
func isOfficialEntry(provider, modelID string) bool {
	id := modelID
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[i+1:] // 去掉 openrouter 之类的 vendor 前缀（如 ~deepseek/deepseek-flash-latest → deepseek-flash-latest）
	}
	modelPrefix := id
	if i := strings.IndexByte(id, '-'); i > 0 {
		modelPrefix = id[:i]
	}
	if modelPrefix == "" {
		return false
	}
	return strings.EqualFold(provider, modelPrefix)
}

// SearchPrices 按关键词（模型 ID 或供应商）模糊搜索最近一次拉取的价格目录。
// 排序优先「官方/直连」条目置顶，再按「模型 ID 升序 → 供应商升序」，方便前端浏览选择。
func (s *Syncer) SearchPrices(q string) []PriceEntry {
	q = strings.ToLower(strings.TrimSpace(q))
	s.pricesMu.RLock()
	defer s.pricesMu.RUnlock()
	out := make([]PriceEntry, 0, len(s.lastPricesFull))
	for _, p := range s.lastPricesFull {
		if q != "" {
			hay := strings.ToLower(p.Provider + " " + p.ID + " " + p.Provider + "/" + p.ID)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, PriceEntry{
			Provider:      p.Provider,
			ModelID:       p.ID,
			InputUSD:      p.Pricing.Prompt,
			OutputUSD:     p.Pricing.Completion,
			CacheUSD:      p.Pricing.CacheRead,
			CacheWriteUSD: p.Pricing.CacheWrite,
			ReasoningUSD:  p.Pricing.Reasoning,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		oi := isOfficialEntry(out[i].Provider, out[i].ModelID)
		oj := isOfficialEntry(out[j].Provider, out[j].ModelID)
		if oi != oj {
			return oi // 官方优先
		}
		if out[i].ModelID != out[j].ModelID {
			return out[i].ModelID < out[j].ModelID
		}
		return out[i].Provider < out[j].Provider
	})
	return out
}

// LastUpdated 返回最近一次成功拉取价格的时间（0 表示未同步过）。
func (s *Syncer) LastUpdated() time.Time {
	s.lastUpdateMu.RLock()
	defer s.lastUpdateMu.RUnlock()
	return s.lastUpdatedAt
}

// ListWatchlist 查询关注列表。
func (s *Syncer) ListWatchlist(ctx context.Context) ([]WatchlistItem, error) {
	return s.store.ListWatchlist(ctx)
}

// UpsertWatchlist 幂等新增/更新关注项（按 external_model_id）。
func (s *Syncer) UpsertWatchlist(ctx context.Context, in *WatchlistItem) (*WatchlistItem, error) {
	return s.store.UpsertWatchlist(ctx, in)
}

// DeleteWatchlist 批量删除关注项。
func (s *Syncer) DeleteWatchlist(ctx context.Context, ids []int64) (int64, error) {
	return s.store.DeleteWatchlist(ctx, ids)
}

// ListAlerts 查询告警（status 空表示全部）。
func (s *Syncer) ListAlerts(ctx context.Context, status string) ([]PriceAlert, error) {
	return s.store.ListAlerts(ctx, status)
}

// ResolveAlert 将告警置为已解决；不存在返回错误。
func (s *Syncer) ResolveAlert(ctx context.Context, id, adminID int64) error {
	return s.store.ResolveAlert(ctx, id, adminID)
}
