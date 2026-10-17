package automodel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"

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

// MemberInput 是 Auto 成员的可写字段。
type MemberInput struct {
	ExternalModelID int64
	Position        int
}

// AutoModelInput 是创建/更新 Auto 的可写字段。
type AutoModelInput struct {
	Name        string
	Scope       string
	OwnerUserID *int64
	Enabled     *bool
	Description string
	Members     []MemberInput
}

// Service 承载 Auto 模型 CRUD 与解析业务逻辑。
type Service struct {
	store *Store
	// extNameExists 跨表重名校验：判断某名称是否已被 external_models.external_name 占用
	// （经 SetExternalNameChecker 由装配层注入；nil=跳过该检查）。
	extNameExists func(ctx context.Context, name string) (bool, error)

	// membersCache 缓存 ResolveMembers（网关每请求按「请求名 + owner」解析 Auto；Auto 低频变更）。
	//   - 数据来源：auto_models + auto_model_members（经 store.GetByName 的作用域可见性过滤 + JOIN）；
	//   - 键 = name + "\x00" + ownerUserID（owner 为 nil 时后缀为空）；值 = 解析结果
	//     （成员列表 + 是否 Auto），含「查无此名 / 已停用」的负结果（isAuto=false）；
	//   - 键首段来自请求体（用户可控），故**有界**（membersCacheMax + FIFO 淘汰）：防止认证用户
	//     以海量随机名打满负缓存造成内存耗尽 DoS。淘汰代价仅为回源一次 DB，不影响正确性。
	//   - 失效点（写路径显式失效，无 TTL）：
	//     本文件 Create/Update/Delete 成功后整体清空（改名/成员变更/删除均可能影响任意键，整体清空避免漏接）；
	//     以及 **跨包** 对外模型写路径（model.Service 经 SetNameWriteHook 回调 InvalidateReadCache），
	//     因解析结果含 JOIN 自 external_models 的 ExternalName/Enabled。
	//   - 关闭开关（SetReadCacheEnabled(false)）时退回直查，不读写缓存。
	membersMu         sync.RWMutex
	membersCache      map[string]resolveCacheEntry
	membersCacheOrder []string
	membersCacheMax   int
	readCacheOff      bool
}

// defaultMembersCacheMax ResolveMembers 读缓存条目上限（键用户可控，必须有界）。
const defaultMembersCacheMax = 100000

// resolveCacheEntry 是 ResolveMembers 的缓存项（含负结果）。
type resolveCacheEntry struct {
	members []Member
	isAuto  bool
}

// membersCacheKey 生成 ResolveMembers 缓存键：name + ownerUserID（owner 为 nil 用空后缀）。
func membersCacheKey(name string, ownerUserID *int64) string {
	if ownerUserID == nil {
		return name + "\x00"
	}
	return name + "\x00" + strconv.FormatInt(*ownerUserID, 10)
}

// invalidateMembersCache 清空 ResolveMembers 缓存（Auto 写路径调用）。
func (s *Service) invalidateMembersCache() {
	s.membersMu.Lock()
	s.membersCache = nil
	s.membersCacheOrder = nil
	s.membersMu.Unlock()
}

// InvalidateReadCache 对外暴露的缓存失效入口：供跨包写路径（model.Service.SetNameWriteHook）
// 在 external_models 变更后调用——因 ResolveMembers 结果含 JOIN 自 external_models 的
// ExternalName/Enabled，模型改名/启停会影响解析结果（P1-4）。
func (s *Service) InvalidateReadCache() { s.invalidateMembersCache() }

// SetReadCacheEnabled 开关 Auto 成员解析读缓存（默认开启）；关闭后 ResolveMembers 直查 DB。
// 由装配层在启动时按配置设置，属于「直查 fallback 安全开关」。
func (s *Service) SetReadCacheEnabled(on bool) { s.readCacheOff = !on }

// NewService 创建 Auto 模型服务。
func NewService(store *Store) *Service {
	return &Service{store: store, membersCacheMax: defaultMembersCacheMax}
}

// SetExternalNameChecker 注入跨表重名校验（装配层传入，查 external_models）。
func (s *Service) SetExternalNameChecker(fn func(ctx context.Context, name string) (bool, error)) {
	s.extNameExists = fn
}

// normalizeEnabled 把可空开关归一为布尔值：nil 时用默认值 def。
func normalizeEnabled(in *bool, def bool) bool {
	if in != nil {
		return *in
	}
	return def
}

// toMembers 把可写成员输入转为领域成员（ID/ExternalName 由存储层回填）。
func toMembers(in []MemberInput) []Member {
	out := make([]Member, 0, len(in))
	for _, m := range in {
		out = append(out, Member{ExternalModelID: m.ExternalModelID, Position: m.Position})
	}
	return out
}

// validateInput 校验并归一化可写字段：名称非空、作用域合法、私有必有归属用户、
// 成员非空且对外模型 ID 合法、排序位唯一；最后按 Position 升序排序保证落库顺序稳定。
// 入参用指针以便把 global 的 OwnerUserID 归一为 nil。
func (s *Service) validateInput(in *AutoModelInput) error {
	if in.Name == "" {
		return errBadRequest("Auto 模型名称不能为空")
	}
	if in.Scope != ScopeGlobal && in.Scope != ScopePrivate {
		return errBadRequest("作用域必须是 global 或 private")
	}
	if in.Scope == ScopePrivate {
		if in.OwnerUserID == nil || *in.OwnerUserID <= 0 {
			return errBadRequest("私有 Auto 必须指定归属用户")
		}
	} else {
		// global 不归属任何用户，忽略传入值。
		in.OwnerUserID = nil
	}
	if len(in.Members) == 0 {
		return errBadRequest("Auto 模型至少需要一个成员")
	}
	seen := make(map[int]bool, len(in.Members))
	for _, m := range in.Members {
		if m.ExternalModelID <= 0 {
			return errBadRequest("成员对外模型 ID 非法")
		}
		if seen[m.Position] {
			return errBadRequest("成员排序位重复")
		}
		seen[m.Position] = true
	}
	// 稳定排序，保证成员顺序与 position 一致。
	sort.SliceStable(in.Members, func(i, j int) bool {
		return in.Members[i].Position < in.Members[j].Position
	})
	return nil
}

// Create 创建 Auto：校验 + 跨表重名 + 同名 Auto + 入库。
func (s *Service) Create(ctx context.Context, in AutoModelInput) (*AutoModel, error) {
	if err := s.validateInput(&in); err != nil {
		return nil, err
	}
	if err := s.checkExternalName(ctx, in.Name); err != nil {
		return nil, err
	}
	// 同名 Auto 预检查（扁平命名空间：跨作用域、跨 owner 一律不许重名，故用 GetByNameAny）；
	// store 层 23505 兜底并发冲突。
	if _, err := s.store.GetByNameAny(ctx, in.Name); err == nil {
		return nil, errConflict("Auto 模型名已存在")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check auto name: %w", err)
	}

	created, err := s.store.Insert(ctx, &AutoModel{
		Name:        in.Name,
		Scope:       in.Scope,
		OwnerUserID: in.OwnerUserID,
		Enabled:     normalizeEnabled(in.Enabled, true),
		Description: in.Description,
		Members:     toMembers(in.Members),
	})
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("Auto 模型名已存在")
		}
		if errors.Is(err, ErrExternalModelMissing) {
			return nil, errBadRequest("成员引用的对外模型不存在")
		}
		if errors.Is(err, ErrOwnerUserMissing) {
			return nil, errBadRequest("指定的归属用户不存在")
		}
		return nil, fmt.Errorf("create auto model: %w", err)
	}
	// 缓存失效：Auto 新增/成员落地后清空解析缓存（写路径显式失效，无 TTL）。
	s.invalidateMembersCache()
	return created, nil
}

// Update 更新 Auto。不存在 → 40401；改名冲突 → 40901；跨作用域变更 → 40001。
func (s *Service) Update(ctx context.Context, id int64, in AutoModelInput) (*AutoModel, error) {
	if id <= 0 {
		return nil, errBadRequest("无效的 Auto 模型 ID")
	}
	if err := s.validateInput(&in); err != nil {
		return nil, err
	}
	existing, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("Auto 模型不存在")
		}
		return nil, fmt.Errorf("get auto model: %w", err)
	}
	// 不允许 global ⇄ private 互转（归属语义与可见性都会随之变化）。
	if in.Scope != existing.Scope {
		return nil, errBadRequest("不允许变更 Auto 模型作用域")
	}
	if in.Name != existing.Name {
		if err := s.checkExternalName(ctx, in.Name); err != nil {
			return nil, err
		}
		// 排除自身：扁平命名空间下用 GetByNameAny 全表查，命中且 ID 不同才算冲突。
		if other, err := s.store.GetByNameAny(ctx, in.Name); err == nil && other.ID != id {
			return nil, errConflict("Auto 模型名已存在")
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("check auto name: %w", err)
		}
	}

	m := &AutoModel{
		ID:          id,
		Name:        in.Name,
		Scope:       in.Scope,
		OwnerUserID: in.OwnerUserID,
		Enabled:     normalizeEnabled(in.Enabled, existing.Enabled),
		Description: in.Description,
		Members:     toMembers(in.Members),
	}
	if err := s.store.Update(ctx, m); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("Auto 模型名已存在")
		}
		if errors.Is(err, ErrExternalModelMissing) {
			return nil, errBadRequest("成员引用的对外模型不存在")
		}
		if errors.Is(err, ErrOwnerUserMissing) {
			return nil, errBadRequest("指定的归属用户不存在")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("Auto 模型不存在")
		}
		return nil, fmt.Errorf("update auto model: %w", err)
	}
	// 缓存失效：改名/成员变更后清空解析缓存（写路径显式失效，无 TTL）。
	s.invalidateMembersCache()
	return s.store.GetByID(ctx, id)
}

// Delete 删除 Auto。不存在 → 40401。
func (s *Service) Delete(ctx context.Context, id int64) error {
	if err := s.store.Delete(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound("Auto 模型不存在")
		}
		return fmt.Errorf("delete auto model: %w", err)
	}
	// 缓存失效：删除后清空解析缓存（写路径显式失效，无 TTL）。
	s.invalidateMembersCache()
	return nil
}

// Get 按主键查询 Auto。不存在 → 40401。
func (s *Service) Get(ctx context.Context, id int64) (*AutoModel, error) {
	m, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("Auto 模型不存在")
		}
		return nil, fmt.Errorf("get auto model: %w", err)
	}
	return m, nil
}

// List 查询 Auto 列表。scope 为空返回全部；ownerUserID 非空时按可见性过滤（global + 该 owner 的 private）。
func (s *Service) List(ctx context.Context, scope string, ownerUserID *int64) ([]AutoModel, error) {
	list, err := s.store.List(ctx, scope, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("list auto models: %w", err)
	}
	return list, nil
}

// ResolveMembers 解析一个「请求模型名」是否为 Auto：
//   - 是 Auto：返回按 position 升序的成员列表 + true + nil
//   - 不是 Auto（查无此名）：返回 nil + false + nil（不报错，交由上层按普通对外模型处理）
//   - 其它错误：返回 nil + false + err
func (s *Service) ResolveMembers(ctx context.Context, name string, ownerUserID *int64) ([]Member, bool, error) {
	key := membersCacheKey(name, ownerUserID)
	if !s.readCacheOff {
		s.membersMu.RLock()
		cached, ok := s.membersCache[key]
		s.membersMu.RUnlock()
		if ok {
			// 防御性拷贝：命中缓存返回副本，调用方就地修改成员切片不得污染缓存（P2-3）。
			return append([]Member(nil), cached.members...), cached.isAuto, nil
		}
	}
	m, err := s.store.GetByName(ctx, name, ownerUserID)
	if errors.Is(err, sql.ErrNoRows) {
		s.cacheResolvedMembers(key, nil, false)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve auto model: %w", err)
	}
	if !m.Enabled {
		// 停用的 Auto 视为不存在，回落普通模型解析。
		s.cacheResolvedMembers(key, nil, false)
		return nil, false, nil
	}
	s.cacheResolvedMembers(key, m.Members, true)
	return m.Members, true, nil
}

// cacheResolvedMembers 写入 ResolveMembers 结果缓存（members 为 nil 表示负结果）。
// 有界（membersCacheMax）：超过上限按插入顺序 FIFO 淘汰最早键，防用户可控键打满内存（P1-5）。
// 防御性拷贝：存入缓存的是成员切片副本，避免调用方后续就地修改其持有的切片污染缓存（P2-3）。
func (s *Service) cacheResolvedMembers(key string, members []Member, isAuto bool) {
	if s.readCacheOff {
		return
	}
	s.membersMu.Lock()
	if s.membersCache == nil {
		s.membersCache = make(map[string]resolveCacheEntry)
	}
	if _, exists := s.membersCache[key]; !exists {
		s.membersCacheOrder = append(s.membersCacheOrder, key)
	}
	s.membersCache[key] = resolveCacheEntry{members: append([]Member(nil), members...), isAuto: isAuto}
	s.evictMembersCacheLocked()
	s.membersMu.Unlock()
}

// evictMembersCacheLocked 超过上限时按插入顺序 FIFO 淘汰最早键（调用方须持有写锁）。
func (s *Service) evictMembersCacheLocked() {
	if s.membersCacheMax <= 0 {
		return
	}
	for len(s.membersCache) > s.membersCacheMax && len(s.membersCacheOrder) > 0 {
		oldest := s.membersCacheOrder[0]
		s.membersCacheOrder = s.membersCacheOrder[1:]
		delete(s.membersCache, oldest)
	}
}

// checkExternalName 跨表重名校验：未注入检查器时跳过；命中则报冲突。
func (s *Service) checkExternalName(ctx context.Context, name string) error {
	if s.extNameExists == nil {
		return nil
	}
	exists, err := s.extNameExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check external name: %w", err)
	}
	if exists {
		return errConflict("模型名已被对外模型占用")
	}
	return nil
}
