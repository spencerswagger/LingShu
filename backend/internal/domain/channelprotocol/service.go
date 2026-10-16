package channelprotocol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// APIError 是带 HTTP 状态码与业务码的领域错误，handler 据此返回响应。
type APIError struct {
	HTTPStatus int
	Code       int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

func errBadRequest(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusBadRequest, Code: resp.CodeBadRequest, Message: msg}
}
func errConflict(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusConflict, Code: resp.CodeConflict, Message: msg}
}
func errNotFound(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: msg}
}

// SyncPending 一项需二次确认才覆盖的条目（已编辑），含可读差异文本。
type SyncPending struct {
	Id     string `json:"Id"`
	Name   string `json:"Name"`
	Diff   string `json:"Diff"`
	Edited bool   `json:"Edited"`
	Stale  bool   `json:"Stale"`
}

// SyncResult 预设同步结果：Pending 为待确认清单，Applied 为本次直接覆盖/新增数。
type SyncResult struct {
	Pending []SyncPending `json:"Pending"`
	Applied int           `json:"Applied"`
}

// ImportResult 渠道协议导入结果：Created 为新增数，Skipped 为去重跳过数，Warnings 为校验失败告警。
type ImportResult struct {
	Created  int      `json:"Created"`
	Skipped  int      `json:"Skipped"`
	Warnings []string `json:"Warnings"`
}

// Service 承载渠道协议业务逻辑（CRUD + 预设物化/同步）。
type Service struct {
	store  *Store
	client *http.Client
	logger *slog.Logger
	// packURL 预设站点地址：为空表示未配置（不拉取，AC-12）。
	packURL string
	// refresher 协议变更后回调（装配层注入）：按协议 ID 刷新引用渠道的模型与运行时；
	// nil 表示未装配（跳过，保持现状）。
	refresher func(ctx context.Context, protocolID int64) error
}

// NewService 创建渠道协议服务。
func NewService(store *Store) *Service {
	return &Service{store: store, client: http.DefaultClient, logger: slog.Default()}
}

// SetPackURL 注入预设站点地址（来自环境变量配置）。
func (s *Service) SetPackURL(url string) { s.packURL = strings.TrimSpace(url) }

// SetChannelRefresher 注入协议变更后的引用渠道刷新钩子（CRUD/Sync/InitialSync 成功后调用）。
// 刷新为最终一致：回调失败仅记日志，不使协议主操作失败。
func (s *Service) SetChannelRefresher(fn func(ctx context.Context, protocolID int64) error) {
	s.refresher = fn
}

// notifyRefresh 以协议 ID 回调刷新钩子（未装配则跳过；失败仅记日志）。
func (s *Service) notifyRefresh(ctx context.Context, protocolID int64) {
	if s.refresher == nil {
		return
	}
	if err := s.refresher(ctx, protocolID); err != nil {
		s.logger.Error("refresh channels for channel protocol failed", "protocol_id", protocolID, "err", err)
	}
}

// SetHTTPClient 注入 HTTP 客户端（测试用）。
func (s *Service) SetHTTPClient(c *http.Client) {
	if c != nil {
		s.client = c
	}
}

// SetLogger 注入日志器。
func (s *Service) SetLogger(l *slog.Logger) {
	if l != nil {
		s.logger = l
	}
}

// List 返回全部渠道协议。
func (s *Service) List(ctx context.Context) ([]ChannelProtocol, error) {
	return s.store.List(ctx)
}

// Get 查询单个渠道协议。
func (s *Service) Get(ctx context.Context, id int64) (*ChannelProtocol, error) {
	cp, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道协议不存在")
		}
		return nil, fmt.Errorf("get channel protocol: %w", err)
	}
	return cp, nil
}

// validate 校验渠道协议字段合法性。
func validate(cp *ChannelProtocol) error {
	if strings.TrimSpace(cp.Name) == "" {
		return errBadRequest("渠道协议名称不能为空")
	}
	if b := strings.TrimSpace(cp.BaseURL); b != "" {
		if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
			return errBadRequest("BaseURL 必须以 http:// 或 https:// 开头")
		}
	}
	defaults := 0
	for name, spec := range cp.Protocols {
		if !IsBaseProtocol(Canonical(name)) {
			return errBadRequest("不支持的基础协议：" + name)
		}
		if spec.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return errBadRequest("同一渠道协议内至多一个默认协议(default)")
	}
	switch strings.ToLower(strings.TrimSpace(cp.RequestHeaders.PassthroughMode)) {
	case "", PassthroughNone, PassthroughWhitelist, PassthroughBlacklist, PassthroughAll:
	default:
		return errBadRequest("passthroughMode 只支持 none / whitelist / blacklist / all")
	}
	// sourceStatus 受 DB CHECK 约束（ACTIVE/STALE）；空值由 store 落库时默认 ACTIVE。
	// 导入携带非法值时在此拦截 → 计入 Warnings 跳过，避免触发 23514 导致整批回滚。
	switch cp.SourceStatus {
	case "", SourceActive, SourceStale:
	default:
		return errBadRequest("sourceStatus 只支持 ACTIVE / STALE")
	}
	return nil
}

// normalizeIn 归一化入参：协议键 canonical 化、策略补默认、名称去空白。
func normalizeIn(cp *ChannelProtocol) {
	cp.Name = strings.TrimSpace(cp.Name)
	cp.BaseURL = strings.TrimSpace(cp.BaseURL)
	cp.Protocols = canonicalProtocols(cp.Protocols)
	cp.RequestHeaders = normalizePolicy(cp.RequestHeaders)
	if cp.Models == nil {
		cp.Models = map[string]ModelGateway{}
	}
}

// Create 手动新建渠道协议（提交即视为已编辑）。
func (s *Service) Create(ctx context.Context, in ChannelProtocol) (*ChannelProtocol, error) {
	cp := in
	normalizeIn(&cp)
	// 服务端掌控来源/编辑字段：先落定再校验，避免客户端传入的非法 sourceStatus 影响 Result。
	cp.ID = 0
	cp.PresetSourceID = ""
	cp.Edited = true
	cp.SourceStatus = SourceActive
	if err := validate(&cp); err != nil {
		return nil, err
	}
	if _, err := s.store.GetByName(ctx, cp.Name); err == nil {
		return nil, errConflict("渠道协议名称已存在")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check channel protocol name: %w", err)
	}
	created, err := s.store.Insert(ctx, &cp)
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("渠道协议名称已存在")
		}
		return nil, fmt.Errorf("create channel protocol: %w", err)
	}
	s.notifyRefresh(ctx, created.ID)
	return created, nil
}

// Update 手动更新渠道协议（提交即视为已编辑，保留预设来源 ID）。
func (s *Service) Update(ctx context.Context, id int64, in ChannelProtocol) (*ChannelProtocol, error) {
	existing, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	cp := in
	normalizeIn(&cp)
	// 服务端掌控来源/编辑字段：先落定再校验，避免客户端传入的非法 sourceStatus 影响 Result。
	cp.ID = id
	cp.PresetSourceID = existing.PresetSourceID
	cp.Edited = true
	cp.SourceStatus = existing.SourceStatus
	cp.CreatedAt = existing.CreatedAt
	if err := validate(&cp); err != nil {
		return nil, err
	}
	if err := s.store.Update(ctx, &cp); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("渠道协议名称已存在")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道协议不存在")
		}
		return nil, fmt.Errorf("update channel protocol: %w", err)
	}
	s.notifyRefresh(ctx, id)
	return &cp, nil
}

// Delete 删除渠道协议；仍被渠道引用时拒绝。
func (s *Service) Delete(ctx context.Context, id int64) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	n, err := s.store.CountChannelRefs(ctx, id)
	if err != nil {
		return fmt.Errorf("count channel refs: %w", err)
	}
	if n > 0 {
		return errConflict("该渠道协议仍被渠道引用，无法删除")
	}
	if err := s.store.Delete(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound("渠道协议不存在")
		}
		return fmt.Errorf("delete channel protocol: %w", err)
	}
	s.notifyRefresh(ctx, id)
	return nil
}

// InitialSync 首次启动（初始化）拉取一次预设包并物化；地址未设置则不拉取（AC-12）。
// 仅「首次启动」执行：以 sys_configs 标记持久化，已同步过则跳过（跨重启幂等）。
// 失败仅记日志、不阻塞启动。返回本次是否完整执行并成功落「已同步」标记（标记失败返回 false，下次启动重试）。
func (s *Service) InitialSync(ctx context.Context) bool {
	if s.packURL == "" {
		s.logger.Info("preset pack url not configured, skip initial channel protocol sync")
		return false
	}
	synced, err := s.store.InitialSynced(ctx)
	if err != nil {
		s.logger.Error("read channel protocol initial-sync flag failed", "err", err)
		return false
	}
	if synced {
		s.logger.Info("initial channel protocol sync skipped (already synced on a previous start)")
		return false
	}
	res, err := s.Sync(ctx, true)
	if err != nil {
		s.logger.Error("initial channel protocol sync failed", "err", err)
		return false
	}
	if err := s.store.MarkInitialSynced(ctx); err != nil {
		// 未持久化标记 → 下次启动会重试（Sync 幂等，安全）。
		s.logger.Error("mark channel protocol initial-synced failed", "err", err)
		return false
	}
	s.logger.Info("initial channel protocol sync done", "applied", res.Applied, "pending", len(res.Pending))
	return true
}

// Sync 拉取预设包并物化渠道协议：
//   - 新增预设条目 → 直接插入（Applied+1）；
//   - 未编辑条目 → 直接覆盖（Applied+1，标记回「同步」）；
//   - 已编辑条目 → confirm=false 时进入 Pending（展示差异，不覆盖）；confirm=true 时覆盖；
//   - 预设中已删除/改名的来源 → 标记「来源已失效(STALE)」，协议不删除。
func (s *Service) Sync(ctx context.Context, confirm bool) (*SyncResult, error) {
	if s.packURL == "" {
		return &SyncResult{}, nil
	}
	pack, err := fetchPack(ctx, s.client, s.packURL)
	if err != nil {
		return nil, err
	}
	existing, err := s.store.ListByPresetSource(ctx)
	if err != nil {
		return nil, err
	}
	bySource := make(map[string]*ChannelProtocol, len(existing))
	for i := range existing {
		bySource[existing[i].PresetSourceID] = &existing[i]
	}

	res := &SyncResult{Pending: make([]SyncPending, 0)}
	seen := make(map[string]bool, len(pack))
	// 固定按来源键升序遍历，保证 Pending 顺序稳定（pack 为 map，range 顺序随机）。
	keys := make([]string, 0, len(pack))
	for k := range pack {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// 全部写入（新增/覆盖/标记 STALE）收敛在同一事务内，保证预设同步原子：
	// 任一条失败整体回滚，避免出现「同步半完成」的中间态。
	var changed []int64 // 本次事务内发生变更的协议 ID（提交成功后回调刷新引用渠道）
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		for _, key := range keys {
			p := pack[key]
			cand := p.toChannelProtocol()
			// 以物化后的 preset_source_id 为唯一键（落库即用它），与 ListByPresetSource 回读一致，
			// 避免旧实现「map 键 vs p.ID」不一致导致重复插入并撞 name 唯一索引。
			if cand.PresetSourceID == "" {
				cand.PresetSourceID = key
			}
			sid := cand.PresetSourceID
			seen[sid] = true
			cur := bySource[sid]
			if cur == nil {
				created, ierr := insertOn(ctx, tx, cand)
				if ierr != nil {
					return fmt.Errorf("insert preset channel protocol %s: %w", sid, ierr)
				}
				changed = append(changed, created.ID)
				res.Applied++
				continue
			}
			diff := diffOf(cur, cand)
			if !cur.Edited {
				if oerr := s.overwriteFromPreset(ctx, tx, cur, cand); oerr != nil {
					return oerr
				}
				changed = append(changed, cur.ID)
				res.Applied++
				continue
			}
			if confirm {
				if oerr := s.overwriteFromPreset(ctx, tx, cur, cand); oerr != nil {
					return oerr
				}
				changed = append(changed, cur.ID)
				res.Applied++
				continue
			}
			res.Pending = append(res.Pending, SyncPending{
				Id: strconv.FormatInt(cur.ID, 10), Name: cur.Name, Diff: diff, Edited: true,
			})
		}

		// 来源已失效：预设中已不存在，标记 STALE（不删除，AC-15）。
		for _, e := range existing {
			if seen[e.PresetSourceID] {
				continue
			}
			if e.SourceStatus == SourceStale {
				continue
			}
			if uerr := updateSourceStatusOn(ctx, tx, e.ID, SourceStale); uerr != nil {
				return fmt.Errorf("mark channel protocol stale id=%d: %w", e.ID, uerr)
			}
			changed = append(changed, e.ID)
			res.Pending = append(res.Pending, SyncPending{
				Id: strconv.FormatInt(e.ID, 10), Name: e.Name, Diff: "预设来源已失效（保留协议）",
				Edited: e.Edited, Stale: true,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, id := range changed {
		s.notifyRefresh(ctx, id)
	}
	return res, nil
}

// Export 把全部渠道协议导出为预设包形状：键优先取 preset_source_id（非空时），否则取 name。
// Protocols / Models 为空时初始化为非 nil 空集合，避免序列化为 null。
func (s *Service) Export(ctx context.Context) (Pack, error) {
	list, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	pack := make(Pack)
	for i := range list {
		cp := &list[i]
		key := cp.PresetSourceID
		if key == "" {
			key = cp.Name
		}
		protocols := cp.Protocols
		if protocols == nil {
			protocols = Protocols{}
		}
		models := make(map[string]Model, len(cp.Models))
		for modelID, mg := range cp.Models {
			models[modelID] = Model{ID: modelID, Gateway: mg}
		}
		pack[key] = Provider{
			ID:     cp.PresetSourceID,
			Name:   cp.Name,
			Models: models,
			Gateway: GatewayBlock{
				BaseURL:        cp.BaseURL,
				Protocols:      protocols,
				RequestHeaders: cp.RequestHeaders,
				Edited:         cp.Edited,
				SourceStatus:   cp.SourceStatus,
			},
		}
	}
	return pack, nil
}

// Import 从预设包形状导入渠道协议：逐条物化、校验、去重后插入，单事务提交。
//   - Edited 为 true / SourceStatus 非空时覆盖物化默认值（否则保留 false / ACTIVE）；
//   - 校验失败 → 追加 Warnings 并跳过（不计入 Created/Skipped）；
//   - name 或非空 preset_source_id 已存在 → Skipped++；
//   - 其余 → 新增（Created++），并登记新 name / preset_source_id 防批内重复。
func (s *Service) Import(ctx context.Context, pack Pack) (*ImportResult, error) {
	existing, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(existing))
	sources := make(map[string]bool, len(existing))
	for i := range existing {
		names[existing[i].Name] = true
		if existing[i].PresetSourceID != "" {
			sources[existing[i].PresetSourceID] = true
		}
	}
	keys := make([]string, 0, len(pack))
	for k := range pack {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	res := &ImportResult{Warnings: make([]string, 0)}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		for _, key := range keys {
			p := pack[key]
			cand := p.toChannelProtocol()
			if p.Gateway.Edited {
				cand.Edited = true
			}
			if p.Gateway.SourceStatus != "" {
				cand.SourceStatus = p.Gateway.SourceStatus
			}
			normalizeIn(cand)
			if verr := validate(cand); verr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("渠道协议 %q：%s", cand.Name, verr.Error()))
				continue
			}
			if names[cand.Name] || (cand.PresetSourceID != "" && sources[cand.PresetSourceID]) {
				res.Skipped++
				continue
			}
			cand.ID = 0
			if _, ierr := insertOn(ctx, tx, cand); ierr != nil {
				return fmt.Errorf("import channel protocol %s: %w", key, ierr)
			}
			res.Created++
			names[cand.Name] = true
			if cand.PresetSourceID != "" {
				sources[cand.PresetSourceID] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// overwriteFromPreset 用预设值覆盖（保留 ID/created_at），编辑标记复位、来源置 ACTIVE（在事务内执行）。
func (s *Service) overwriteFromPreset(ctx context.Context, tx *sql.Tx, cur, cand *ChannelProtocol) error {
	next := *cand
	next.ID = cur.ID
	next.CreatedAt = cur.CreatedAt
	next.Edited = false
	next.SourceStatus = SourceActive
	if err := validate(&next); err != nil {
		return err
	}
	if err := updateOn(ctx, tx, &next); err != nil {
		return fmt.Errorf("update preset channel protocol %d: %w", cur.ID, err)
	}
	return nil
}

// diffOf 生成两次配置的可读差异摘要（供已编辑条目二次确认展示）。
func diffOf(cur, cand *ChannelProtocol) string {
	var b strings.Builder
	if cur.BaseURL != cand.BaseURL {
		fmt.Fprintf(&b, "BaseURL: %q → %q\n", cur.BaseURL, cand.BaseURL)
	}
	if !sameJSON(cur.Protocols, cand.Protocols) {
		fmt.Fprintf(&b, "Protocols: %s → %s\n", mustJSON(cur.Protocols), mustJSON(cand.Protocols))
	}
	if !sameJSON(cur.RequestHeaders, cand.RequestHeaders) {
		fmt.Fprintf(&b, "RequestHeaders: %s → %s\n", mustJSON(cur.RequestHeaders), mustJSON(cand.RequestHeaders))
	}
	if !sameJSON(cur.Models, cand.Models) {
		fmt.Fprintf(&b, "Models: %s → %s\n", mustJSON(cur.Models), mustJSON(cand.Models))
	}
	if b.Len() == 0 {
		return "无差异"
	}
	return strings.TrimRight(b.String(), "\n")
}

func sameJSON(a, b any) bool {
	return string(mustJSON(a)) == string(mustJSON(b))
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
