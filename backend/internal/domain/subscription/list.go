package subscription

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// Filter 是 S1 的查询条件（与 View 的 getSubscriptions 参数同名）。
//
// Status/ShareCost 是**派生值**，SQL 里过滤不了（状态不落列），所以在服务层过滤 + 分页。
type Filter struct {
	Provider  string
	Account   string
	Status    string
	ShareCost *bool
	Page      int
	Size      int
}

// ListView 是 S1 的响应体（View 的 SubscriptionView：Items + Total）。
type ListView struct {
	Items []*View `json:"Items"`
	// Total 是**过滤后**的总数（前端分页器要用），不是本页条数。
	Total int `json:"Total"`
	Page  int `json:"Page"`
	Size  int `json:"Size"`
}

// FormOptions 是 S7 的候选（View 的 SubscriptionFormOptions）。
type FormOptions struct {
	Providers []string `json:"Providers"`
	Accounts  []string `json:"Accounts"`
	// CnyRate / R 为 null 表示「未配置」：录入页据此禁用 USD（缺汇率必 422）。
	CnyRate *float64 `json:"CnyRate"`
	R       *int64   `json:"R"`
}

const (
	defaultPageSize = 20
	maxPageSize     = 100
	formAccountHint = 50 // 账号软候选最多 50 个（够补全，不至于把几千个账号全下发）
)

// List 实现 S1：列表 + 派生状态 + 本期已摊/未摊出。
func (s *Service) List(ctx context.Context, f Filter) (*ListView, *Error) {
	if s == nil || s.store == nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: "订阅服务未装配存储"}
	}
	rows, err := s.store.List(ctx)
	if err != nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: err.Error()}
	}
	// 折算参数读不到不是错误：列表照样能看，只是 Price 显示为 0、判定退回「价格未折算」口径。
	r, rerr := s.r()
	if rerr != nil {
		r = 0
	}
	cnyRate, cerr := s.cnyRate()
	if cerr != nil {
		cnyRate = 0
	}

	provider := strings.TrimSpace(f.Provider)
	account := strings.TrimSpace(f.Account)
	status := strings.ToLower(strings.TrimSpace(f.Status))
	items := make([]*View, 0, len(rows))
	for i := range rows {
		row := rows[i].Row
		if provider != "" && row.Provider != provider {
			continue
		}
		if account != "" && row.Account != account {
			continue
		}
		if f.ShareCost != nil && row.ShareCost != *f.ShareCost {
			continue
		}
		var allocated *float64
		// HasCycle=false（账期缺失）时本期窗口无意义 → 两个字段保持 null。
		if rows[i].HasCycle {
			a := rows[i].Allocated
			allocated = &a
		}
		view := s.render(&row, allocated, r, cnyRate, nil)
		if status != "" && view.Status != status {
			continue
		}
		items = append(items, view)
	}

	total := len(items)
	page, size := f.Page, f.Size
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return &ListView{Items: items[start:end], Total: total, Page: page, Size: size}, nil
}

// FormOptions 实现 S7：录入页候选。
//
// provider 来自拓扑镜像（不在镜像里的录入必 400 provider_not_found，所以只能选）；
// 账号是软候选（近 30 天账单/账本里出现过），可手输，未命中只是 Warnings。
func (s *Service) FormOptions(ctx context.Context) (*FormOptions, *Error) {
	if s == nil || s.store == nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: "订阅服务未装配存储"}
	}
	providers, err := s.store.Providers(ctx)
	if err != nil {
		return nil, &Error{Status: 500, Code: 50001, Reason: ReasonInternal, Message: err.Error()}
	}
	if providers == nil {
		providers = []string{}
	}

	out := &FormOptions{Providers: providers, Accounts: []string{}}
	known, err := s.store.KnownAccounts(ctx, s.now().AddDate(0, 0, -30))
	if err != nil {
		// 候选缺失不阻断录入（账号可手输），但要让运营知道这次没取到。
		s.log.Warn("读取账号候选失败（候选为空）", "err", err)
	} else {
		accounts := make([]string, 0, len(known))
		for a := range known {
			accounts = append(accounts, a)
		}
		sort.Strings(accounts)
		if len(accounts) > formAccountHint {
			accounts = accounts[:formAccountHint]
		}
		out.Accounts = accounts
	}

	if r, err := s.r(); err == nil {
		out.R = &r
	}
	if rate, err := s.cnyRate(); err == nil && rate > 0 {
		out.CnyRate = &rate
	}
	return out, nil
}

// parseFilter 把 query 参数解析成 Filter（非法数字按默认值处理，不 400：
// 列表页多一个坏参数不该整页打不开，这与录入的严格校验是两套口径）。
func parseFilter(q map[string][]string) Filter {
	first := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	atoi := func(s string) int {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0
		}
		return n
	}
	f := Filter{
		Provider: first("Provider"),
		Account:  first("Account"),
		Status:   first("Status"),
		Page:     atoi(first("Page")),
		Size:     atoi(first("Size")),
	}
	if v := strings.TrimSpace(first("ShareCost")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			f.ShareCost = &b
		}
	}
	return f
}
