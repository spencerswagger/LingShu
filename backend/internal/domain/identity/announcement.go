// announcement.go 系统公告领域实现：存储、服务与 HTTP 处理器。
// 数据模型对齐 db/migrations/0001_init.sql 中 announcements 表。
// publish_at 为 nil 视为立即发布；dev 端仅返回「已发布且未过期且 enabled」的有效公告。
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 公告级别。
const (
	AnnounceLevelInfo    = "info"
	AnnounceLevelWarning = "warning"
	AnnounceLevelDanger  = "danger"
)

// Announcement 对应 announcements 表一行。
type Announcement struct {
	ID        int64
	Title     string
	Content   string
	Level     string
	PublishAt *time.Time
	ExpireAt  *time.Time
	Enabled   bool
	CreatedBy *int64
	CreatedAt time.Time
}

const announceCols = `id, title, content, level, publish_at, expire_at, enabled, created_by, created_at`

// AnnouncementStore 提供 announcements 表数据访问。
type AnnouncementStore struct {
	db *sql.DB
}

// NewAnnouncementStore 创建公告存储。
func NewAnnouncementStore(db *sql.DB) *AnnouncementStore {
	return &AnnouncementStore{db: db}
}

func scanAnnouncement(row interface{ Scan(...any) error }) (*Announcement, error) {
	var a Announcement
	var pubAt, expAt sql.NullTime
	var createdBy sql.NullInt64
	err := row.Scan(&a.ID, &a.Title, &a.Content, &a.Level, &pubAt, &expAt,
		&a.Enabled, &createdBy, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if pubAt.Valid {
		t := pubAt.Time
		a.PublishAt = &t
	}
	if expAt.Valid {
		t := expAt.Time
		a.ExpireAt = &t
	}
	if createdBy.Valid {
		v := createdBy.Int64
		a.CreatedBy = &v
	}
	return &a, nil
}

// Create 插入公告并回填主键与创建时间。
func (s *AnnouncementStore) Create(ctx context.Context, a *Announcement) (*Announcement, error) {
	if a.ID == 0 {
		a.ID = idgen.New()
	}
	var createdBy any
	if a.CreatedBy != nil {
		createdBy = *a.CreatedBy
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO announcements(id, title, content, level, publish_at, expire_at, enabled, created_by)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+announceCols,
		a.ID, a.Title, a.Content, a.Level, a.PublishAt, a.ExpireAt, a.Enabled, createdBy)
	created, err := scanAnnouncement(row)
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Update 更新公告可变字段；不存在返回 sql.ErrNoRows。
func (s *AnnouncementStore) Update(ctx context.Context, a *Announcement) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE announcements SET title=$1, content=$2, level=$3, publish_at=$4, expire_at=$5, enabled=$6
		 WHERE id=$7`,
		a.Title, a.Content, a.Level, a.PublishAt, a.ExpireAt, a.Enabled, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetByID 按主键查询；不存在返回 sql.ErrNoRows。
func (s *AnnouncementStore) GetByID(ctx context.Context, id int64) (*Announcement, error) {
	return scanAnnouncement(s.db.QueryRowContext(ctx,
		`SELECT `+announceCols+` FROM announcements WHERE id=$1 AND deleted_at IS NULL`, id))
}

// List 查询公告。effectiveOnly 为真时仅返回有效公告：
// enabled=true 且 (publish_at 为空或 <=now) 且 (expire_at 为空或 >now)。
func (s *AnnouncementStore) List(ctx context.Context, effectiveOnly bool, now time.Time) ([]Announcement, error) {
	query := `SELECT ` + announceCols + ` FROM announcements WHERE deleted_at IS NULL`
	args := []any{}
	if effectiveOnly {
		query += ` AND enabled = true
		          AND (publish_at IS NULL OR publish_at <= $1)
		          AND (expire_at IS NULL OR expire_at > $1)`
		args = append(args, now)
	}
	query += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list announcements: %w", err)
	}
	defer rows.Close()
	list := make([]Announcement, 0, 8)
	for rows.Next() {
		a, err := scanAnnouncement(rows)
		if err != nil {
			return nil, fmt.Errorf("scan announcement: %w", err)
		}
		list = append(list, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// BatchDelete 批量删除公告，返回删除数量。
func (s *AnnouncementStore) BatchDelete(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	res, err := s.db.ExecContext(ctx,
		`UPDATE announcements SET deleted_at = now() WHERE id = ANY($1::bigint[])`, anyArg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AnnouncementService 承载公告业务逻辑。
type AnnouncementService struct {
	store *AnnouncementStore
	now   func() time.Time
}

// NewAnnouncementService 创建公告服务。
func NewAnnouncementService(store *AnnouncementStore) *AnnouncementService {
	return &AnnouncementService{store: store, now: time.Now}
}

func errAnnouncementNotFound() *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "公告不存在"}
}

// List admin 端全量查询。
func (s *AnnouncementService) List(ctx context.Context) ([]Announcement, error) {
	return s.store.List(ctx, false, s.now())
}

// ListEffective dev 端仅返回当前有效公告。
func (s *AnnouncementService) ListEffective(ctx context.Context) ([]Announcement, error) {
	return s.store.List(ctx, true, s.now())
}

// Create 创建公告。
func (s *AnnouncementService) Create(ctx context.Context, in Announcement, createdBy int64) (*Announcement, error) {
	if in.Title == "" {
		return nil, errBadRequest("公告标题不能为空")
	}
	if in.Content == "" {
		return nil, errBadRequest("公告内容不能为空")
	}
	if !validLevel(in.Level) {
		return nil, errBadRequest("公告级别只允许 info/warning/danger")
	}
	enabled := in.Enabled
	cb := createdBy
	a := &Announcement{
		Title:     in.Title,
		Content:   in.Content,
		Level:     in.Level,
		PublishAt: in.PublishAt,
		ExpireAt:  in.ExpireAt,
		Enabled:   enabled,
		CreatedBy: &cb,
	}
	created, err := s.store.Create(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("create announcement: %w", err)
	}
	return created, nil
}

// Update 更新公告；不存在返回 40401。
func (s *AnnouncementService) Update(ctx context.Context, id int64, in Announcement) (*Announcement, error) {
	if in.Title == "" {
		return nil, errBadRequest("公告标题不能为空")
	}
	if in.Content == "" {
		return nil, errBadRequest("公告内容不能为空")
	}
	if !validLevel(in.Level) {
		return nil, errBadRequest("公告级别只允许 info/warning/danger")
	}
	existing, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errAnnouncementNotFound()
		}
		return nil, fmt.Errorf("get announcement: %w", err)
	}
	upd := &Announcement{
		ID:        id,
		Title:     in.Title,
		Content:   in.Content,
		Level:     in.Level,
		PublishAt: in.PublishAt,
		ExpireAt:  in.ExpireAt,
		Enabled:   in.Enabled,
	}
	if err := s.store.Update(ctx, upd); err != nil {
		return nil, fmt.Errorf("update announcement: %w", err)
	}
	upd.CreatedAt = existing.CreatedAt
	upd.CreatedBy = existing.CreatedBy
	return upd, nil
}

// BatchDelete 批量删除公告。
func (s *AnnouncementService) BatchDelete(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, errBadRequest("未指定要删除的公告")
	}
	n, err := s.store.BatchDelete(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("batch delete announcements: %w", err)
	}
	return n, nil
}

func validLevel(l string) bool {
	return l == AnnounceLevelInfo || l == AnnounceLevelWarning || l == AnnounceLevelDanger
}

// AnnouncementHandler 暴露公告 HTTP 处理器。
type AnnouncementHandler struct {
	svc        *AnnouncementService
	userIDFrom func(ctx context.Context) (int64, bool)
}

// NewAnnouncementHandler 创建公告处理器。
func NewAnnouncementHandler(svc *AnnouncementService, userIDFrom func(ctx context.Context) (int64, bool)) *AnnouncementHandler {
	return &AnnouncementHandler{svc: svc, userIDFrom: userIDFrom}
}

type announcementResponse struct {
	ID        int64     `json:"ID,string"`
	Title     string    `json:"Title"`
	Content   string    `json:"Content"`
	Level     string    `json:"Level"`
	PublishAt *string   `json:"PublishAt,omitempty"`
	ExpireAt  *string   `json:"ExpireAt,omitempty"`
	Enabled   bool      `json:"Enabled"`
	CreatedAt time.Time `json:"CreatedAt"`
}

func fmtTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	v := t.Format("2006-01-02T15:04:05Z07:00")
	return &v
}

func (a *Announcement) response() announcementResponse {
	return announcementResponse{
		ID:        a.ID,
		Title:     a.Title,
		Content:   a.Content,
		Level:     a.Level,
		PublishAt: fmtTime(a.PublishAt),
		ExpireAt:  fmtTime(a.ExpireAt),
		Enabled:   a.Enabled,
		CreatedAt: a.CreatedAt,
	}
}

type announcementInput struct {
	Title     string     `json:"Title"`
	Content   string     `json:"Content"`
	Level     string     `json:"Level"`
	PublishAt *time.Time `json:"PublishAt"`
	ExpireAt  *time.Time `json:"ExpireAt"`
	Enabled   *bool      `json:"Enabled"`
}

func (h *AnnouncementHandler) parseInput(w http.ResponseWriter, r *http.Request) (Announcement, bool) {
	var req announcementInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return Announcement{}, false
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return Announcement{
		Title:     req.Title,
		Content:   req.Content,
		Level:     req.Level,
		PublishAt: req.PublishAt,
		ExpireAt:  req.ExpireAt,
		Enabled:   enabled,
	}, true
}

// HandleAdminList GET /api/v1/admin/announcements
func (h *AnnouncementHandler) HandleAdminList(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	items := make([]announcementResponse, 0, len(list))
	for i := range list {
		items = append(items, list[i].response())
	}
	resp.OK(w, r, map[string]any{"List": items})
}

// HandleAdminCreate POST /api/v1/admin/announcements
func (h *AnnouncementHandler) HandleAdminCreate(w http.ResponseWriter, r *http.Request) {
	in, ok := h.parseInput(w, r)
	if !ok {
		return
	}
	creator, _ := h.userIDFrom(r.Context())
	a, err := h.svc.Create(r.Context(), in, creator)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, a.response())
}

// HandleAdminUpdate PUT /api/v1/admin/announcements/{id}
func (h *AnnouncementHandler) HandleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的公告 ID")
		return
	}
	in, ok := h.parseInput(w, r)
	if !ok {
		return
	}
	a, err := h.svc.Update(r.Context(), id, in)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, a.response())
}

// HandleAdminBatchDelete POST /api/v1/admin/announcements/batch-delete
func (h *AnnouncementHandler) HandleAdminBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs idgen.IDs `json:"IDs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	n, err := h.svc.BatchDelete(r.Context(), []int64(req.IDs))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]int64{"Deleted": n})
}

// HandleDevList GET /api/v1/dev/announcements 返回当前有效公告。
func (h *AnnouncementHandler) HandleDevList(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListEffective(r.Context())
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	items := make([]announcementResponse, 0, len(list))
	for i := range list {
		items = append(items, list[i].response())
	}
	resp.OK(w, r, map[string]any{"List": items})
}
