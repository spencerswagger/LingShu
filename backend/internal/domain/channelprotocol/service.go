package channelprotocol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// Service 承载渠道协议业务逻辑（CRUD + 预设物化/同步）。
type Service struct {
	store  *Store
	client *http.Client
	logger *slog.Logger
	// packURL 预设站点地址：为空表示未配置（不拉取，AC-12）。
	packURL string
	// initialized 标记首次启动拉取是否已执行（只首次拉取一次）。
	initialized bool
}

// NewService 创建渠道协议服务。
func NewService(store *Store) *Service {
	return &Service{store: store, client: http.DefaultClient, logger: slog.Default()}
}

// SetPackURL 注入预设站点地址（来自环境变量配置）。
func (s *Service) SetPackURL(url string) { s.packURL = strings.TrimSpace(url) }

// PackURL 返回预设站点地址。
func (s *Service) PackURL() string { return s.packURL }

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
	if h := strings.ToLower(strings.TrimSpace(cp.Auth.Header)); h != "" {
		switch h {
		case "authorization", "x-api-key", "api-key":
		default:
			return errBadRequest("auth.header 只支持 Authorization / x-api-key / api-key")
		}
	}
	if sc := strings.ToLower(strings.TrimSpace(cp.Auth.Scheme)); sc != "" {
		switch sc {
		case "bearer", "raw":
		default:
			return errBadRequest("auth.scheme 只支持 bearer / raw")
		}
	}
	switch strings.ToLower(strings.TrimSpace(cp.RequestHeaders.PassthroughMode)) {
	case "", PassthroughNone, PassthroughWhitelist, PassthroughBlacklist, PassthroughAll:
	default:
		return errBadRequest("passthroughMode 只支持 none / whitelist / blacklist / all")
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
	if err := validate(&cp); err != nil {
		return nil, err
	}
	if _, err := s.store.GetByName(ctx, cp.Name); err == nil {
		return nil, errConflict("渠道协议名称已存在")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check channel protocol name: %w", err)
	}
	cp.ID = 0
	cp.PresetSourceID = ""
	cp.Edited = true
	cp.SourceStatus = SourceActive
	created, err := s.store.Insert(ctx, &cp)
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("渠道协议名称已存在")
		}
		return nil, fmt.Errorf("create channel protocol: %w", err)
	}
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
	if err := validate(&cp); err != nil {
		return nil, err
	}
	cp.ID = id
	cp.PresetSourceID = existing.PresetSourceID
	cp.Edited = true
	cp.SourceStatus = existing.SourceStatus
	cp.CreatedAt = existing.CreatedAt
	if err := s.store.Update(ctx, &cp); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("渠道协议名称已存在")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道协议不存在")
		}
		return nil, fmt.Errorf("update channel protocol: %w", err)
	}
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
	return nil
}

// Initialized 返回首次启动拉取是否已执行。
func (s *Service) Initialized() bool { return s.initialized }

// InitialSync 首次启动（初始化）拉取一次预设包并物化；地址未设置则不拉取（AC-12）。
// 失败仅记日志、不阻塞启动。返回是否实际执行了拉取。
func (s *Service) InitialSync(ctx context.Context) bool {
	if s.packURL == "" {
		s.logger.Info("preset pack url not configured, skip initial channel protocol sync")
		return false
	}
	res, err := s.Sync(ctx, true)
	if err != nil {
		s.logger.Error("initial channel protocol sync failed", "err", err)
		return false
	}
	s.initialized = true
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
	for id, p := range pack {
		seen[id] = true
		cand := p.toChannelProtocol()
		cur := bySource[id]
		if cur == nil {
			if _, err := s.store.Insert(ctx, cand); err != nil {
				return nil, fmt.Errorf("insert preset channel protocol %s: %w", id, err)
			}
			res.Applied++
			continue
		}
		diff := diffOf(cur, cand)
		if !cur.Edited {
			if err := s.overwriteFromPreset(ctx, cur, cand); err != nil {
				return nil, err
			}
			res.Applied++
			continue
		}
		if confirm {
			if err := s.overwriteFromPreset(ctx, cur, cand); err != nil {
				return nil, err
			}
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
		if err := s.store.UpdateSourceStatus(ctx, e.ID, SourceStale); err != nil {
			s.logger.Error("mark channel protocol stale failed", "id", e.ID, "err", err)
			continue
		}
		res.Pending = append(res.Pending, SyncPending{
			Id: strconv.FormatInt(e.ID, 10), Name: e.Name, Diff: "预设来源已失效（保留协议）",
			Edited: e.Edited, Stale: true,
		})
	}
	return res, nil
}

// overwriteFromPreset 用预设值覆盖（保留 ID/created_at），编辑标记复位、来源置 ACTIVE。
func (s *Service) overwriteFromPreset(ctx context.Context, cur, cand *ChannelProtocol) error {
	next := *cand
	next.ID = cur.ID
	next.CreatedAt = cur.CreatedAt
	next.Edited = false
	next.SourceStatus = SourceActive
	if err := validate(&next); err != nil {
		return err
	}
	if err := s.store.Update(ctx, &next); err != nil {
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
	if !sameJSON(cur.Auth, cand.Auth) {
		fmt.Fprintf(&b, "Auth: %s → %s\n", mustJSON(cur.Auth), mustJSON(cand.Auth))
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
