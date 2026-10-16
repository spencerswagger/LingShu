package console

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// ServiceStatusView 是「服务状态」页聚合视图：渠道三层运行明细 + 对外模型清单 + 按需展开的候选顺序 +
// 成本比较样值。ID 类字段用 `,string` JSON tag，与既有 ChannelView/ModelView 契约一致。
type ServiceStatusView struct {
	DataTime           time.Time                  `json:"DataTime"`
	Channels           []channel.ChannelDetail    `json:"Channels"`
	ExternalModels     []externalModelItem        `json:"ExternalModels"`
	Candidates         map[string][]candidateItem `json:"Candidates"`
	CandidatesDataTime *time.Time                 `json:"CandidatesDataTime,omitempty"`
	SampleUsage        billing.Usage              `json:"SampleUsage"`
}

// externalModelItem 对外模型清单项（只读枚举，供前端选择要展开的候选）。
type externalModelItem struct {
	ID           int64  `json:"ID,string"`
	ExternalName string `json:"ExternalName"`
	Enabled      bool   `json:"Enabled"`
}

// candidateItem 单个路由候选（按选区顺序排列，Rank 从 1 递增）。
type candidateItem struct {
	ChannelID       int64   `json:"ChannelID,string"`
	ChannelKeyID    int64   `json:"ChannelKeyID,string"`
	InternalModelID string  `json:"InternalModelID"`
	ModelRowID      int64   `json:"ModelRowID,string"`
	ExternalModelID int64   `json:"ExternalModelID,string"`
	Rank            int     `json:"Rank"`
	Cost            float64 `json:"Cost"`
	CostKnown       bool    `json:"CostKnown"`
	TimeCoeff       float64 `json:"TimeCoeff"`
}

// HandleServiceStatus GET /api/v1/admin/service-status
//
// 聚合端点：一次性返回渠道三层运行明细、对外模型清单、按需展开的候选顺序与成本比较样值。
// DataTime 为最终一致快照时间（决策 13）：各层明细允许与路由热路径毫秒级错位，前端自行对齐展示。
// 候选顺序按需计算（决策 14）：仅当传入 Expand（逗号分隔的对外模型 ID 列表）且非空时才逐项调用
// RouteAll，且 SessionKey 传空以关闭会话亲和；未传 Expand 时不做任何路由计算。
//
// 密钥存活会话数（Keys[].ActiveSessions）为「实时读取」路由会话注册表所得，非快照：
// 与 MaxSessions（并发上限，静态配置）语义不同，前者是当前占用、后者是容量；未装配路由引擎时为 0。
func (h *Admin) HandleServiceStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	dataTime := h.now()

	view := ServiceStatusView{
		DataTime:   dataTime,
		Channels:   h.channelMgr.RuntimeDetails(),
		Candidates: make(map[string][]candidateItem),
	}

	// 密钥存活会话数回填：实时读取会话注册表（非快照）；未装配路由引擎时保持 0。
	if h.routeEng != nil {
		for ci := range view.Channels {
			for ki := range view.Channels[ci].Keys {
				k := &view.Channels[ci].Keys[ki]
				k.ActiveSessions = h.routeEng.ActiveKeySessions(k.View.KeyID)
			}
		}
	}

	// 对外模型清单：未注入模型源时为空切片（非 nil）。
	view.ExternalModels = make([]externalModelItem, 0)
	if h.models != nil {
		models, err := h.models.ListModels(ctx, nil)
		if err != nil {
			slog.Default().Error("读取对外模型失败", "request_id", resp.RequestID(r), "err", err)
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取服务状态失败")
			return
		}
		for i := range models {
			view.ExternalModels = append(view.ExternalModels, externalModelItem{
				ID:           models[i].ID,
				ExternalName: models[i].ExternalName,
				Enabled:      models[i].Enabled,
			})
		}
	}

	// 一致性收敛（不允许隐性逻辑）：渠道运行时快照只在启动/渠道编辑/协议同步时重建，
	// 删除对外模型不会通知渠道 Manager，故仍引用已删对外模型的渠道内部模型会残留在快照里。
	// 这里以「未删除的对外模型清单（List 自带 deleted_at IS NULL）」为白名单，剔除对外模型已
	// 不存在的内部模型行，保证展示与库表删除语义一致。
	// 仅当「已注入模型源」时才过滤：未注入（h.models==nil）无法判定存在性，跳过以免全部误删；
	// 已注入但清单为空（全部对外模型均已删除）属合法信号，照常过滤以清空残留。
	if h.models != nil {
		active := make(map[int64]struct{}, len(view.ExternalModels))
		for _, m := range view.ExternalModels {
			active[m.ID] = struct{}{}
		}
		for ci := range view.Channels {
			models := view.Channels[ci].Models[:0]
			for _, md := range view.Channels[ci].Models {
				if _, ok := active[md.View.ExternalModelID]; ok {
					models = append(models, md)
				}
			}
			view.Channels[ci].Models = models
			// 渠道无独立状态机，State 为「全部内部模型与密钥的最差值」；Models 已被裁剪，
			// 必须据此重算，否则状态灯（取 State）会与提示（遍历已裁剪的 Models）自相矛盾。
			view.Channels[ci].State = worstChannelState(models, view.Channels[ci].Keys)
		}
	}

	// 候选顺序：仅当 Expand 非空且已注入路由引擎时按需计算。
	if expand := parseExpand(r.URL.Query().Get("Expand")); len(expand) > 0 && h.routeEng != nil {
		nameByID := make(map[int64]string, len(view.ExternalModels))
		for _, m := range view.ExternalModels {
			nameByID[m.ID] = m.ExternalName
		}
		for _, id := range expand {
			name, ok := nameByID[id]
			if !ok {
				slog.Default().Warn("展开的对外模型不存在", "request_id", resp.RequestID(r), "external_model_id", id)
				continue
			}
			routes, err := h.routeEng.RouteAll(ctx, router.RouteSpec{ExternalModel: name, SessionKey: ""})
			if err != nil {
				slog.Default().Error("计算候选顺序失败", "request_id", resp.RequestID(r), "external_model_id", id, "err", err)
				continue
			}
			items := make([]candidateItem, 0, len(routes))
			for i, rt := range routes {
				items = append(items, candidateItem{
					ChannelID:       rt.ChannelID,
					ChannelKeyID:    rt.ChannelKeyID,
					InternalModelID: rt.InternalModelID,
					ModelRowID:      rt.ModelRowID,
					ExternalModelID: rt.ExternalModelID,
					Rank:            i + 1,
					Cost:            rt.Cost,
					CostKnown:       rt.CostKnown,
					TimeCoeff:       rt.TimeCoeff,
				})
			}
			view.Candidates[strconv.FormatInt(id, 10)] = items
		}
		cdt := dataTime
		view.CandidatesDataTime = &cdt
	}

	// 成本比较样值：与路由成本比较同源（未注入引擎时为零值）。
	if h.routeEng != nil {
		view.SampleUsage = h.routeEng.CostSample()
	}

	resp.OK(w, r, view)
}

// parseExpand 解析逗号分隔的对外模型 ID 列表；忽略空白项与非数字项。
// 非法项（非数字）跳过并记录日志（不整请求失败）。
func parseExpand(v string) []int64 {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			slog.Default().Warn("忽略非法的 Expand 项", "value", p)
			continue
		}
		out = append(out, id)
	}
	return out
}

// worstChannelState 取渠道派生状态：渠道无独立状态机，取其全部内部模型与密钥的最差状态
// （DISABLED > DRAIN > NORMAL）；空参为 NORMAL。语义与 channel.worstState 一致，
// 供 console 在裁剪 Models 后重算 ChannelDetail.State（channel.worstState 未导出）。
func worstChannelState(models []channel.ModelDetail, keys []channel.KeyDetail) channel.State {
	w := channel.StateNormal
	for i := range models {
		switch models[i].View.State {
		case channel.StateDisabled:
			return channel.StateDisabled
		case channel.StateDrain:
			w = channel.StateDrain
		}
	}
	for i := range keys {
		switch keys[i].View.State {
		case channel.StateDisabled:
			return channel.StateDisabled
		case channel.StateDrain:
			w = channel.StateDrain
		}
	}
	return w
}
