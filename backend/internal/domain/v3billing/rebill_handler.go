package v3billing

// rebill_handler.go：unpriced 账单补收的管理端接口（B 方案·手动触发）。
//
//	POST /api/v1/admin/billings/rebill-unpriced   {"Ref": "group/auto-flash"}
//
// 归属 poller 的方法集（Rebill 依赖其 DB/售价解析/计费依赖），handler 单独成文件只为可读。
// 响应字段 PascalCase，与 View 的 `src/api/v2.ts` 既有视图约定一致。
//
// 响应形态（毒行不堵整批，评审 P2-1）：
//   - 整批走完、部分行成功：**200 + RebillResult**（六字段契约不变；失败明细是附加字段
//     Failed，无失败时 omitempty 不出现）。运营在响应里同时看到「已扣条数/金额」与
//     「失败行+原因」，修复后重放补齐（幂等三层保证不重复扣款）。
//   - 400 只留给「整批没有开始」的形态：Ref 缺失/非法、该组当前没有售价（ErrRebillGroupUnpriced）。
//     行级失败不再 400 —— 那会把已成功扣款的部分结果整个丢掉，运营看不到已扣了什么。
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
	ReasonRebillGroupUnpriced = "rebill_group_unpriced"
	// ReasonRebillRowUnresolvable 行级失败键：按当前配置仍解析不到售价（进 Failed 明细）。
	ReasonRebillRowUnresolvable = "rebill_row_unresolvable"
	// ReasonRebillNoWallet 行级失败键：原行用户没有积分钱包，无法扣款（进 Failed 明细；
	// Record 前拦截，避免 billing 失败通道落 failed 残单堵死重放）。
	ReasonRebillNoWallet = "rebill_no_wallet"
	// ReasonRebillBrokenRow 扫描层毒行：tokens 损坏等，该行进不了计价（进 Failed 明细）。
	ReasonRebillBrokenRow = "rebill_broken_row"
	// ReasonRebillRecordError 计费/落库层错误（余额不足、标记守卫拒绝等，进 Failed 明细）。
	ReasonRebillRecordError = "rebill_record_error"
	// ReasonRebillAlreadyRebilled 行级失败里撞上「已被补收」的形态（进 Failed 明细）。
	ReasonRebillAlreadyRebilled = "rebill_already_rebilled"
)

// RebillHandler 是补收接口（nil = 不挂载）。
type RebillHandler struct {
	poller *Poller
}

// NewRebillHandler 创建补收接口处理器。
func NewRebillHandler(p *Poller) *RebillHandler { return &RebillHandler{poller: p} }

// HandleRebillUnpriced POST /api/v1/admin/billings/rebill-unpriced
// 对指定组执行补收：扫描其历史 unpriced 账单，按当前售价逐条开新账单并扣款。
// 逐行失败隔离：失败行带原因进响应明细（RebillResult.Failed），批次照常走完。
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
