package tag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

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
func errInternal() *APIError {
	return &APIError{HTTPStatus: http.StatusInternalServerError, Code: resp.CodeInternalError, Message: "服务器内部错误"}
}

// TagInput 创建/更新标签的可写字段。
type TagInput struct {
	Name        string
	Description string
	KVPairs     map[string]string
	Enabled     *bool
}

// Service 承载语义标签 CRUD 业务逻辑。
type Service struct {
	store *Store
}

// NewService 创建标签服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// validateBase 校验 name 非空且 KV 键非空。
func (s *Service) validateBase(in TagInput) error {
	if in.Name == "" {
		return errBadRequest("标签名称不能为空")
	}
	for k := range in.KVPairs {
		if k == "" {
			return errBadRequest("标签键不能为空")
		}
	}
	return nil
}

// CreateTag 创建标签：校验 + 名称唯一 + 入库。
// creatorID 由调用方（admin handler）从鉴权上下文注入；0 表示系统/无用户操作。
func (s *Service) CreateTag(ctx context.Context, in TagInput, creatorID int64) (*Tag, error) {
	if err := s.validateBase(in); err != nil {
		return nil, err
	}
	if _, err := s.store.GetByName(ctx, in.Name); err == nil {
		return nil, errConflict("标签名称已存在")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check tag name: %w", err)
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	created, err := s.store.Insert(ctx, &Tag{
		Name:        in.Name,
		Description: in.Description,
		KVPairs:     in.KVPairs,
		Enabled:     enabled,
		CreatedBy:   creatorID,
	})
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("标签名称已存在")
		}
		return nil, fmt.Errorf("create tag: %w", err)
	}
	return created, nil
}

// UpdateTag 更新标签。不存在 → 40401；name 冲突 → 40901。
func (s *Service) UpdateTag(ctx context.Context, id int64, in TagInput) (*Tag, error) {
	if err := s.validateBase(in); err != nil {
		return nil, err
	}
	existing, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("标签不存在")
		}
		return nil, fmt.Errorf("get tag: %w", err)
	}
	if in.Name != existing.Name {
		if other, err := s.store.GetByName(ctx, in.Name); err == nil && other.ID != id {
			return nil, errConflict("标签名称已存在")
		}
	}

	enabled := existing.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	t := &Tag{
		ID:          id,
		Name:        in.Name,
		Description: in.Description,
		KVPairs:     in.KVPairs,
		Enabled:     enabled,
	}
	if err := s.store.Update(ctx, t); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("标签名称已存在")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("标签不存在")
		}
		return nil, fmt.Errorf("update tag: %w", err)
	}
	return s.store.GetByID(ctx, id)
}

// ListTags 按 enabled 过滤查询。
func (s *Service) ListTags(ctx context.Context, enabled *bool) ([]Tag, error) {
	tags, err := s.store.List(ctx, enabled)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	return tags, nil
}

// Resolve 校验并返回给定 ID 的启用标签；任一不存在或停用即报错。
func (s *Service) Resolve(ctx context.Context, ids []int64) ([]Tag, error) {
	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	tags, err := s.store.ListByIDs(ctx, uniq)
	if err != nil {
		return nil, fmt.Errorf("resolve tags: %w", err)
	}
	if len(tags) != len(uniq) {
		return nil, errBadRequest("部分标签不存在，无法绑定")
	}
	for i := range tags {
		if !tags[i].Enabled {
			return nil, errBadRequest("标签「"+tags[i].Name+"」已停用，无法绑定")
		}
	}
	return tags, nil
}

// BatchDeleteTags 批量删除标签。被令牌引用的标签无法删除 → 40901。
func (s *Service) BatchDeleteTags(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, errBadRequest("未指定要删除的标签")
	}
	// 软删除下不再依赖 DB 外键拦截：业务预检查引用。
	refs, err := s.store.CountTokenRefs(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("count token refs: %w", err)
	}
	if refs > 0 {
		return 0, errConflict("标签正被令牌引用，无法删除")
	}
	chRefs, err := s.store.CountChannelRefs(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("count channel refs: %w", err)
	}
	if chRefs > 0 {
		return 0, errConflict("标签正被渠道引用，无法删除")
	}
	n, err := s.store.BatchDelete(ctx, ids)
	if err != nil {
		if errors.Is(err, ErrTagInUse) {
			return 0, errConflict("标签正被令牌引用，无法删除")
		}
		return 0, fmt.Errorf("batch delete tags: %w", err)
	}
	return n, nil
}
