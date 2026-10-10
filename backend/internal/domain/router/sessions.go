package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Session 一次客户端会话（token:model:x-session-id 维度）。
// 会话同时携带身份信息（UserID/TokenID/TokenDisplay）与运行时归属（渠道密钥 ChannelKeyID / 内部模型）。
// D3 起收口：ChannelKeyID 为会话绑定/计数/持久化的唯一维度，不再有 ChannelID 过渡字段与回退逻辑。
type Session struct {
	SessionID       string // 会话指纹（哈希），日志关联/计数去重/防串号
	UserID          int64  // 用户
	TokenID         int64
	TokenDisplay    string // 密钥展示名 sk-gw-***（不存明文 key）
	Model           string // 对外模型名
	SessionRaw      string // 客户端原始 x-session-id
	Name            string // 可读名称（首条用户消息摘要或客户端自定义）
	ChannelKeyID    int64  // 会话绑定的渠道密钥 ID（计数与持久化的唯一维度）
	InternalModelID string // 绑定的内部模型 ID
	CreatedAt       time.Time
	LastActive      time.Time
	ExpireAt        time.Time
	Closed          bool       // 已关闭：不再被路由命中、释放并发槽，但保留在列表（管理可见）
	ClosedAt        *time.Time // 关闭时间（nil=未关闭）
	// LastMsgFingerprints 该会话上次请求的每消息短 hash，用于 call_log 增量 diff
	//（进程内存字段，不落库；DB 恢复后为空时网关降级为「最后一条 user + tool 链」）。
	LastMsgFingerprints []string
}

// SessionID 计算会话指纹：sha256("llmgw-session:"+user+":"+token+":"+model+":"+raw)。
func SessionID(userID, tokenID int64, model, sessionRaw string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("llmgw-session:%d:%d:%s:%s", userID, tokenID, model, sessionRaw)))
	return hex.EncodeToString(h[:])
}

// sessionPersister 会话持久化投影接口（SessionStore 满足；测试注入内存 fake）。
type sessionPersister interface {
	Upsert(ctx context.Context, sess *Session) error
	Delete(ctx context.Context, sessionID string) error
	DeleteMany(ctx context.Context, ids []string) error
	SetClosed(ctx context.Context, sessionID string, closed bool) error
	LoadActive(ctx context.Context, now time.Time) ([]*Session, error)
}

// SessionRegistry 会话注册表（进程内存，DB 为持久化投影）：
//   - 会话生命周期：创建/续期（滑动 TTL）/过期（周期清扫）/踢下线（Kill/KillByFilter）
//   - 按渠道密钥存活会话计数（byKy）：max_concurrent 判定新会话创建（并发=存活会话数，非请求数）
//   - 持久化钩子：Attach（新建/迁移）与 Renew → Upsert；Sweep 过期 → DeleteMany；Kill → Delete；
//     DB 操作失败仅记日志，不影响热路径（内存为准，最终一致）
//
// 排空（渠道或模型）允许既有会话续行；禁用连既有会话也拒绝（由路由层判定）。
type SessionRegistry struct {
	mu    sync.Mutex
	byKey map[string]*Session       // sessionID → 会话
	byKy  map[int64]map[string]bool // 渠道密钥 keyID → {sessionID}（B4 起由 byCh 迁移）
	store sessionPersister          // 持久化投影（nil=仅内存，兼容现有调用方）
	logf  func(format string, args ...any)
	now   func() time.Time
	stop  chan struct{}
	// keyChannelOf 可选的 keyID→channelID 映射（装配层注入；CountActiveByChannel 按渠道聚合依赖）。
	// 返回 0 表示该 key 无渠道归属（不参与任何渠道聚合）。
	keyChannelOf func(keyID int64) int64
}

// NewSessionRegistry 创建会话注册表。
func NewSessionRegistry(now func() time.Time) *SessionRegistry {
	if now == nil {
		now = time.Now
	}
	return &SessionRegistry{
		byKey: make(map[string]*Session),
		byKy:  make(map[int64]map[string]bool),
		now:   now,
		stop:  make(chan struct{}),
		logf:  defaultSessionLogf,
	}
}

// defaultSessionLogf 会话持久化失败的默认日志输出（与历史 log.Printf 同语义，改为 slog）。
func defaultSessionLogf(format string, args ...any) {
	slog.Default().Info(fmt.Sprintf(format, args...))
}

// SetStore 注入持久化投影存储（nil=禁用持久化，仅内存；现有调用方兼容）。
func (r *SessionRegistry) SetStore(s sessionPersister) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = s
}

// SetKeyChannelOf 注入 keyID→channelID 映射（nil=禁用按渠道聚合，CountActiveByChannel 返回 0）。
// 装配层（main）可注入 manager.KeyChannelID 实现精确聚合。
func (r *SessionRegistry) SetKeyChannelOf(fn func(keyID int64) int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keyChannelOf = fn
}

// SetLogger 注入持久化失败日志函数（nil 关闭日志；默认 slog.Info）。
func (r *SessionRegistry) SetLogger(fn func(format string, args ...any)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn == nil {
		r.logf = func(string, ...any) {}
	} else {
		r.logf = fn
	}
}

// Lookup 返回未过期会话（含已关闭的，供管理端改名/展示）；过期条目清除。
func (r *SessionRegistry) Lookup(sessionID string) (*Session, bool) {
	if sessionID == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byKey[sessionID]
	if !ok {
		return nil, false
	}
	if r.now().After(s.ExpireAt) {
		r.removeLocked(sessionID, s.ChannelKeyID)
		return nil, false
	}
	return s, true
}

// Hit 判断会话是否可被路由粘性命中（存在、未过期、未关闭）。
// 关闭的会话从路由候选消失；Lookup 仍保留用于管理端。
func (r *SessionRegistry) Hit(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byKey[sessionID]
	if !ok || s.Closed {
		return false
	}
	if r.now().After(s.ExpireAt) {
		r.removeLocked(sessionID, s.ChannelKeyID)
		return false
	}
	return true
}

// Attach 绑定/续期会话到 (key, model)：
//   - 已存在且同密钥 → 续期（不新增计数），返回 (true, false)
//   - 不存在 → 该密钥存活会话数 < maxConcurrent 才创建并 +1，返回 (true, true)
//   - 已存在但密钥不同 → 迁移（旧密钥 -1，新密钥校验后 +1），返回 (true, true)
//   - 存活会话数达上限 → (false, false)（网关据此 failover 下一候选）
//
// maxConcurrent 按 ChannelKeyID 校验；创建/迁移时写 DB 投影（失败仅记日志）。
func (r *SessionRegistry) Attach(s *Session, maxConcurrent int) (ok, created bool) {
	if s == nil || s.SessionID == "" {
		return false, false
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1 << 30
	}
	t := r.now()
	keyID := s.ChannelKeyID

	r.mu.Lock()
	prev, exist := r.byKey[s.SessionID]
	if exist && r.now().After(prev.ExpireAt) {
		r.removeLocked(prev.SessionID, prev.ChannelKeyID)
		exist = false
	}
	if exist {
		// 已关闭的会话视为不存在：重新激活（同名新会话继续占用并发槽）
		if prev.Closed {
			r.removeLocked(prev.SessionID, prev.ChannelKeyID)
			exist = false
		}
	}
	if exist {
		// 同密钥 → 续期（成功转发后由 Renew 落库，这里只更新内存）
		if prev.ChannelKeyID == keyID {
			prev.LastActive = t
			prev.ExpireAt = s.ExpireAt
			prev.InternalModelID = s.InternalModelID
			r.mu.Unlock()
			return true, false
		}
		// 密钥迁移：释放旧密钥计数，按新密钥重新校验
		r.removeLocked(prev.SessionID, prev.ChannelKeyID)
	}
	if len(r.byKy[keyID]) >= maxConcurrent {
		r.mu.Unlock()
		return false, false
	}
	s.CreatedAt = t
	s.LastActive = t
	if s.ExpireAt.IsZero() {
		s.ExpireAt = t
	}
	r.byKey[s.SessionID] = s
	if r.byKy[keyID] == nil {
		r.byKy[keyID] = make(map[string]bool)
	}
	r.byKy[keyID][s.SessionID] = true
	store, logf := r.store, r.logf
	r.mu.Unlock()

	// 持久化投影（新建/迁移）：失败仅记日志，内存为准。
	if store != nil {
		if err := store.Upsert(context.Background(), s); err != nil {
			logf("session upsert %s: %v", s.SessionID, err)
		}
	}
	return true, true
}

// Renew 续期现存会话并落库（成功转发后调用；不改密钥归属）。已关闭会话不续期。
func (r *SessionRegistry) Renew(sessionID string, ttl time.Duration) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	s, ok := r.byKey[sessionID]
	if !ok || s.Closed {
		r.mu.Unlock()
		return
	}
	s.LastActive = r.now()
	if ttl > 0 {
		s.ExpireAt = s.LastActive.Add(ttl)
	}
	store, logf := r.store, r.logf
	r.mu.Unlock()

	if store != nil {
		if err := store.Upsert(context.Background(), s); err != nil {
			logf("session renew upsert %s: %v", sessionID, err)
		}
	}
}

// CountActive 返回某渠道密钥当前存活会话数（按 ChannelKeyID 计数）。
// 按渠道聚合请用 CountActiveByChannel。
func (r *SessionRegistry) CountActive(keyID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byKy[keyID])
}

// CountActiveByChannel 聚合某渠道全部密钥的存活会话数（供聚合展示）。
// 会话只携带 ChannelKeyID，按渠道聚合需经 keyID→channel 映射（keyChannelOf）反查归属渠道；
// 未注入映射时返回 0（依赖装配层注入，见 SetKeyChannelOf）。
func (r *SessionRegistry) CountActiveByChannel(channelID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keyChannelOf == nil {
		return 0
	}
	n := 0
	for _, s := range r.byKey {
		if r.keyChannelOf(s.ChannelKeyID) == channelID {
			n++
		}
	}
	return n
}

// SweepNow 清理过期会话并回算计数；批量删除 DB 投影（周期清扫调用）。
func (r *SessionRegistry) SweepNow() {
	t := r.now()
	r.mu.Lock()
	var expired []string
	for id, s := range r.byKey {
		if t.After(s.ExpireAt) {
			r.removeLocked(id, s.ChannelKeyID)
			expired = append(expired, id)
		}
	}
	store, logf := r.store, r.logf
	r.mu.Unlock()

	if store != nil && len(expired) > 0 {
		if err := store.DeleteMany(context.Background(), expired); err != nil {
			logf("sweep delete sessions: %v", err)
		}
	}
}

// Close 关闭单个会话：不再被路由命中、释放并发槽，但保留在注册表与 DB（管理列表仍可见）。
// 与 Kill（真删除）的区别：关闭保留记录，仅停止服务。
func (r *SessionRegistry) Close(sessionID string) bool {
	r.mu.Lock()
	s, ok := r.byKey[sessionID]
	if !ok || s.Closed {
		r.mu.Unlock()
		return false
	}
	s.Closed = true
	t := r.now()
	s.ClosedAt = &t
	// 释放并发槽（计数清除；byKey 保留供列表）
	if ks, ok := r.byKy[s.ChannelKeyID]; ok {
		delete(ks, sessionID)
		if len(ks) == 0 {
			delete(r.byKy, s.ChannelKeyID)
		}
	}
	store, logf := r.store, r.logf
	r.mu.Unlock()

	if store != nil {
		if err := store.SetClosed(context.Background(), sessionID, true); err != nil {
			logf("session close persist %s: %v", sessionID, err)
		}
	}
	return true
}

// CloseByFilter 按用户/令牌/密钥维度批量关闭会话（三条件任一非零即启用该维度，多条件并存=AND）。
// 全部为零视为无筛选，不做任何操作。返回关闭条数（记录保留，仅清除并发槽与路由命中）。
func (r *SessionRegistry) CloseByFilter(userID, tokenID, keyID int64) int {
	if userID == 0 && tokenID == 0 && keyID == 0 {
		return 0
	}
	r.mu.Lock()
	var closed []*Session
	for id, s := range r.byKey {
		if s.Closed {
			continue
		}
		if (userID == 0 || s.UserID == userID) &&
			(tokenID == 0 || s.TokenID == tokenID) &&
			(keyID == 0 || s.ChannelKeyID == keyID) {
			s.Closed = true
			t := r.now()
			s.ClosedAt = &t
			if ks, ok := r.byKy[s.ChannelKeyID]; ok {
				delete(ks, id)
				if len(ks) == 0 {
					delete(r.byKy, s.ChannelKeyID)
				}
			}
			closed = append(closed, s)
		}
	}
	store, logf := r.store, r.logf
	r.mu.Unlock()

	if store != nil {
		for _, s := range closed {
			if err := store.SetClosed(context.Background(), s.SessionID, true); err != nil {
				logf("close by filter persist %s: %v", s.SessionID, err)
				break
			}
		}
	}
	return len(closed)
}

// Kill 删除单个会话（内存 + DB）。密钥删除联动的硬清理用 Kill；管理端「关闭」用 Close。
func (r *SessionRegistry) Kill(sessionID string) bool {
	r.mu.Lock()
	s, ok := r.byKey[sessionID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	r.removeLocked(sessionID, s.ChannelKeyID)
	store, logf := r.store, r.logf
	r.mu.Unlock()

	if store != nil {
		if err := store.Delete(context.Background(), sessionID); err != nil {
			logf("kill delete session %s: %v", sessionID, err)
		}
	}
	return true
}

// KillByFilter 按用户/令牌/密钥维度批量踢下线（三个条件任一非零即启用该维度，多条件并存=AND）。
// 全部为零视为无筛选，不做任何操作。返回删除条数；内存删除后批量删除 DB 投影。
func (r *SessionRegistry) KillByFilter(userID, tokenID, keyID int64) int {
	if userID == 0 && tokenID == 0 && keyID == 0 {
		return 0
	}
	r.mu.Lock()
	var removed []string
	for id, s := range r.byKey {
		if (userID == 0 || s.UserID == userID) &&
			(tokenID == 0 || s.TokenID == tokenID) &&
			(keyID == 0 || s.ChannelKeyID == keyID) {
			r.removeLocked(id, s.ChannelKeyID)
			removed = append(removed, id)
		}
	}
	store, logf := r.store, r.logf
	r.mu.Unlock()

	if store != nil && len(removed) > 0 {
		if err := store.DeleteMany(context.Background(), removed); err != nil {
			logf("kill by filter delete %d sessions: %v", len(removed), err)
		}
	}
	return len(removed)
}

// ListAll 返回当前在线会话的内存快照（防御性副本）。
func (r *SessionRegistry) ListAll() []*Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Session, 0, len(r.byKey))
	for _, s := range r.byKey {
		cp := *s
		out = append(out, &cp)
	}
	return out
}

// LoadPersisted 启动恢复：从 DB 加载未过期会话重建 byKey/byKy（粘性路由 + 并发计数）。
// 恢复的会话仅携带 ChannelKeyID（sessions 表无 channel_id 列），粘性路由按密钥粒度命中。
func (r *SessionRegistry) LoadPersisted(ctx context.Context) error {
	r.mu.Lock()
	store := r.store
	r.mu.Unlock()
	if store == nil {
		return nil
	}
	rows, err := store.LoadActive(ctx, r.now())
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range rows {
		r.byKey[s.SessionID] = s
		if s.Closed {
			continue // 已关闭：保留记录但不占用并发槽、不参与路由
		}
		if r.byKy[s.ChannelKeyID] == nil {
			r.byKy[s.ChannelKeyID] = make(map[string]bool)
		}
		r.byKy[s.ChannelKeyID][s.SessionID] = true
	}
	return nil
}

// removeLocked 删除会话并释放其密钥计数（调用方持锁）。
func (r *SessionRegistry) removeLocked(sessionID string, keyID int64) {
	delete(r.byKey, sessionID)
	if ks, ok := r.byKy[keyID]; ok {
		delete(ks, sessionID)
		if len(ks) == 0 {
			delete(r.byKy, keyID)
		}
	}
}

// Run 周期性清扫过期会话，直到 ctx 取消。
func (r *SessionRegistry) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.SweepNow()
		}
	}
}

// Stop 停止清扫（保留条目，供进程退出）。
func (r *SessionRegistry) Stop() {}
