package v3billing

// rebill_handler.go：unpriced 账单补收的管理端接口（B 方案·手动触发）。
//
//	POST /api/v1/admin/billings/rebill-unpriced   {"Ref": "group/auto-flash"}
//
// 归属 poller 的方法集（Rebill 依赖其 DB/售价解析/计费依赖），handler 单独成文件只为可读。
// 响应字段 PascalCase，与 View 的 `src/api/v2.ts` 既有视图约定一致。
//
// 纪律：本端点与阶段三运营端点同口径 —— 只挂 /api/v1/admin（成本侧数据不给开发者端），
// 鉴权由 server 的 WithAuth(RoleAdmin) 前缀守卫承担。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// Reason 键（前端按 Reason 分支，Code 只判大类 —— 订阅录入标准 §6.2 口径）。
const (
	ReasonRebillGroupUnpriced   = "rebill_group_unpriced"
	ReasonRebillRowUnresolvable = "rebill_row_unresolvable"
)

// RebillHandler 是补收接口（nil = 不挂载）。
type RebillHandler struct {
	poller *Poller
}

// NewRebillHandler 创建补收接口处理器。
func NewRebillHandler(p *Poller) *RebillHandler { return &RebillHandler{poller: p} }

// HandleRebillUnpriced POST /api/v1/admin/billings/rebill-unpriced
// 对指定组执行补收：扫描其历史 unpriced 账单，按当前售价逐条开新账单并扣款。
func (h *RebillHandler) HandleRebillUnpriced(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.poller == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "补收服务未装配")
		return
	}
	var in struct {
		Ref string `json:"Ref"`
	}
	if err := decodeRebillBody(r, &in); err != nil {
		resp.ErrReason(w, r, http.StatusBadRequest, resp.CodeBadRequest, "invalid_field",
			"请求体必须是 JSON 对象且含 Ref 字段："+err.Error(), nil)
		return
	}
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		resp.ErrReason(w, r, http.StatusBadRequest, resp.CodeBadRequest, "invalid_field",
			"缺少 Ref（组 ref / 组 id / auto/<model> 形态）", nil)
		return
	}
	out, err := h.poller.Rebill(r.Context(), ref)
	if err != nil {
		switch {
		case errors.Is(err, ErrRebillGroupUnpriced):
			resp.ErrReason(w, r, http.StatusBadRequest, resp.CodeBadRequest, ReasonRebillGroupUnpriced,
				err.Error(), map[string]any{"Ref": ref})
		case errors.Is(err, ErrRebillRowUnresolvable):
			resp.ErrReason(w, r, http.StatusBadRequest, resp.CodeBadRequest, ReasonRebillRowUnresolvable,
				err.Error(), map[string]any{"Ref": ref})
		default:
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		}
		return
	}
	resp.OK(w, r, out)
}

// decodeRebillBody 限长解码请求体（防大包口径）。
func decodeRebillBody(r *http.Request, into any) error {
	defer func() { _ = r.Body.Close() }()
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(into)
}
