// magpie_model_test.go 覆盖 V2 第一期新增列 external_models.magpie_model 的读写与校验口径：
//   - 非空即「可转发」，nil 即「未映射」（NULL 与空串归一，只有一种「未映射」表示）；
//   - 写入前 trim，非法字符/超长直接 400，不静默丢弃；
//   - 两个对外名映射同一代理模型 → 409（唯一索引 uq_external_models_magpie_model 的语义出口）。
package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
)

func strPtr(s string) *string { return &s }

// TestScanExternal_MagpieModelNormalization 断言扫描口径：NULL / 空串 / 空白串都归一为 nil。
func TestScanExternal_MagpieModelNormalization(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		raw  any
		want *string
	}{
		{name: "NULL → 未映射", raw: nil, want: nil},
		{name: "空串 → 未映射", raw: "", want: nil},
		{name: "空白串 → 未映射", raw: "   ", want: nil},
		{name: "有值 → 去空白保留", raw: "  anthropic/claude-sonnet-4  ", want: strPtr("anthropic/claude-sonnet-4")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, mock := mockService(t)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).WithArgs(int64(1)).
				WillReturnRows(sqlmock.NewRows(extCols).
					AddRow(int64(1), "m", "", true, `{"input":1}`, nil, nil, `[]`, c.raw, nil, now, now))
			m, err := s.store.GetByID(context.Background(), 1)
			if err != nil {
				t.Fatalf("get by id: %v", err)
			}
			if c.want == nil {
				if m.MagpieModel != nil {
					t.Fatalf("期望未映射(nil)，得到 %q", *m.MagpieModel)
				}
				return
			}
			if m.MagpieModel == nil || *m.MagpieModel != *c.want {
				t.Fatalf("magpie_model = %v, 期望 %q", m.MagpieModel, *c.want)
			}
		})
	}
}

// TestValidateMagpieModel 断言写入前校验：非法值 400，不静默落库。
func TestValidateMagpieModel(t *testing.T) {
	long := ""
	for i := 0; i < 129; i++ {
		long += "a"
	}
	cases := []struct {
		name    string
		in      *string
		wantErr bool
	}{
		{name: "nil 允许（不映射）"},
		{name: "空串允许（等价不映射）", in: strPtr("")},
		{name: "正常标识符", in: strPtr("gpt-4o-2024-11-20")},
		{name: "带斜杠", in: strPtr("openrouter/anthropic/claude-3.5")},
		{name: "带冒号", in: strPtr("models/gemini-2.0-flash:generateContent")},
		{name: "含空格拒绝", in: strPtr("gpt 4o"), wantErr: true},
		{name: "含制表符拒绝", in: strPtr("gpt\t4o"), wantErr: true},
		{name: "超长拒绝", in: &long, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateMagpieModel(c.in)
			if c.wantErr {
				if !isBadRequest(err) {
					t.Fatalf("期望 400，得到 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通过，得到 %v", err)
			}
		})
	}
}

// TestCreateModel_SetsMagpieModel 断言创建时把映射（trim 后）写进 INSERT 参数。
func TestCreateModel_SetsMagpieModel(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs("tenant-model").WillReturnError(sql.ErrNoRows)
	expectAliasOtherNone(mock, "tenant-model", 0)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers, aliases, magpie_model) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+cols)).
		WithArgs(sqlmock.AnyArg(), "tenant-model", "", true, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg(), "upstream/gpt-4o").
		WillReturnRows(extRow(&ExternalModel{
			ID: 1, ExternalName: "tenant-model", Enabled: true,
			MagpieModel: strPtr("upstream/gpt-4o"), CreatedAt: now, UpdatedAt: now,
		}, `{"input":1}`))

	created, err := s.CreateModel(context.Background(), ExternalModelInput{
		ExternalName: "tenant-model", SaleRates: saleRates(),
		MagpieModel: strPtr("  upstream/gpt-4o  "),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.MagpieModel == nil || *created.MagpieModel != "upstream/gpt-4o" {
		t.Fatalf("回包 magpie_model = %v, 期望 upstream/gpt-4o", created.MagpieModel)
	}
}

// TestUpdateModel_MagpieModelConflict 断言「同一代理模型被两个对外名映射」→ 409 可读冲突，
// 而不是 500：库层唯一索引是并发兜底，服务层要点名原因。
func TestUpdateModel_MagpieModelConflict(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "mine", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET external_name=$1`)).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: "uq_external_models_magpie_model"})

	_, err := s.UpdateModel(context.Background(), 1, ExternalModelInput{
		ExternalName: "mine", SaleRates: saleRates(), MagpieModel: strPtr("dup-model"),
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 409 {
		t.Fatalf("期望 409 冲突，得到 %v", err)
	}
	if apiErr.Message != "该代理模型已被其他对外模型映射" {
		t.Fatalf("冲突文案应点名代理模型，得到 %q", apiErr.Message)
	}
}

// TestMapUniqueViolation_RoutesByConstraint 断言唯一冲突按约束名分流：
// magpie_model 走 ErrMagpieModelExists，其余（含对外名）仍是 ErrNameExists，非 23505 不映射。
func TestMapUniqueViolation_RoutesByConstraint(t *testing.T) {
	if e := mapUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "uq_external_models_magpie_model"}); !errors.Is(e, ErrMagpieModelExists) {
		t.Fatalf("期望 ErrMagpieModelExists，得到 %v", e)
	}
	if e := mapUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "external_models_external_name_key"}); !errors.Is(e, ErrNameExists) {
		t.Fatalf("期望 ErrNameExists，得到 %v", e)
	}
	if e := mapUniqueViolation(sql.ErrNoRows); e != nil {
		t.Fatalf("非 23505 不应映射，得到 %v", e)
	}
}

// TestViewModels_MagpieModelOnlyOnAdmin 断言「代理侧映射只给管理端」：
// /api/v1/dev/models 复用同一服务，若对外视图带上 magpie_model 就把内部代号漏给租户。
func TestViewModels_MagpieModelOnlyOnAdmin(t *testing.T) {
	now := time.Now()
	m := &ExternalModel{ID: 1, ExternalName: "tenant-gpt", Enabled: true,
		SaleRates: map[string]float64{"input": 1}, MagpieModel: strPtr("openai/gpt-4o"), CreatedAt: now, UpdatedAt: now}

	adminRaw, err := json.Marshal(toAdminView(m))
	if err != nil {
		t.Fatalf("marshal admin view: %v", err)
	}
	if !strings.Contains(string(adminRaw), "openai/gpt-4o") || !strings.Contains(string(adminRaw), `"MagpieModel"`) {
		t.Fatalf("管理端视图应带代理映射: %s", adminRaw)
	}

	devRaw, err := json.Marshal(toView(m))
	if err != nil {
		t.Fatalf("marshal dev view: %v", err)
	}
	for _, leak := range []string{"openai/gpt-4o", "MagpieModel", "magpie_model", "magpie"} {
		if strings.Contains(string(devRaw), leak) {
			t.Fatalf("对外视图泄漏内部映射（%s）: %s", leak, devRaw)
		}
	}
}

// TestAdminView_BoundInternalRefAlwaysPresent 是「内部模型绑定」列的契约守门用例。
//
// 背景：`external_models.bound_internal_ref` 是 0019 加的绑定真源（0024 保留、索引也在），
// 但管理端模型列表一度**不返回该字段**（cols 里没有它）→ 前端 `hasBinding` 判 false，
// 「内部模型绑定」列与「未绑定 · 不售卖」红色横幅永不出现，未绑定模型会被当成正常售卖。
//
// 两条不变量：
//  1. 字段必须**始终存在**（未绑定给 null，不是省略）—— 前端靠「字段存在」决定渲不渲染；
//  2. 未绑定与已绑定必须可区分（null vs ref 字符串），空串按未绑定归一。
func TestAdminView_BoundInternalRefAlwaysPresent(t *testing.T) {
	base := func() *ExternalModel {
		return &ExternalModel{ID: 1, ExternalName: "tenant-gpt", Enabled: true,
			SaleRates: map[string]float64{"input": 1}, MagpieModel: strPtr("openai/gpt-4o")}
	}

	// 已绑定：回 ref 原文。
	bound := base()
	bound.BoundInternalRef = strPtr("openai/gpt-4o")
	raw, err := json.Marshal(toAdminView(bound))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := got["BoundInternalRef"]; !ok || v != "openai/gpt-4o" {
		t.Fatalf("已绑定的管理端视图应带 BoundInternalRef=openai/gpt-4o，实际 %v（键存在=%v）", v, ok)
	}

	// 未绑定：字段必须在，值为 null。
	rawUnbound, err := json.Marshal(toAdminView(base()))
	if err != nil {
		t.Fatalf("marshal unbound: %v", err)
	}
	var gotUnbound map[string]any
	if err := json.Unmarshal(rawUnbound, &gotUnbound); err != nil {
		t.Fatalf("unmarshal unbound: %v", err)
	}
	if v, ok := gotUnbound["BoundInternalRef"]; !ok {
		t.Fatalf("未绑定时字段也必须存在（前端靠字段存在决定是否渲染该列），实际缺失: %s", rawUnbound)
	} else if v != nil {
		t.Fatalf("未绑定时应为 null，实际 %v", v)
	}

	// 对外视图仍然不带该字段：绑定是内部拓扑，属管理端信息。
	devRaw, err := json.Marshal(toView(bound))
	if err != nil {
		t.Fatalf("marshal dev view: %v", err)
	}
	if strings.Contains(string(devRaw), "BoundInternalRef") || strings.Contains(string(devRaw), "bound_internal_ref") {
		t.Fatalf("对外视图不该出现内部绑定字段: %s", devRaw)
	}
}

// TestScanExternal_BoundInternalRefNormalization 断言 DB 值到模型字段的归一：
// NULL / 空串 / 纯空白 → nil（未绑定只有一种表示），有值则 trim 后保留。
func TestScanExternal_BoundInternalRefNormalization(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want *string
	}{
		{name: "null", raw: nil, want: nil},
		{name: "empty", raw: "", want: nil},
		{name: "blank", raw: "   ", want: nil},
		{name: "value", raw: " anthropic/claude-sonnet-4 ", want: strPtr("anthropic/claude-sonnet-4")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			now := time.Now()
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).WithArgs(int64(1)).
				WillReturnRows(sqlmock.NewRows(extCols).
					AddRow(int64(1), "m", "", true, `{"input":1}`, nil, nil, `[]`, nil, c.raw, now, now))

			store := NewStore(db)
			got, err := store.GetByID(context.Background(), 1)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if c.want == nil {
				if got.BoundInternalRef != nil {
					t.Fatalf("%s：应为「未绑定」(nil)，实际 %q", c.name, *got.BoundInternalRef)
				}
				return
			}
			if got.BoundInternalRef == nil || *got.BoundInternalRef != *c.want {
				t.Fatalf("%s：应归一为 %q，实际 %v", c.name, *c.want, got.BoundInternalRef)
			}
		})
	}
}

// TestColsMatchExtCols 是「SELECT 列清单 ↔ 扫描目标 ↔ 测试行构造」三方对齐的护栏。
//
// cols 是拼进 SQL 的字符串、scanExternal 是逐个目标 Scan、extCols 是 sqlmock 的行列名，
// 三处各自维护；漏改一处时单测可能因为「sqlmock 用同一个 cols 拼期望」而侥幸全绿，
// 到真库才 42703/列错位。这里把「列数」这一维固定下来。
func TestColsMatchExtCols(t *testing.T) {
	parts := strings.Split(cols, ",")
	if len(parts) != len(extCols) {
		t.Fatalf("cols 有 %d 列，extCols 有 %d 列 —— 两处必须同步（否则真库列错位）", len(parts), len(extCols))
	}
	for i, p := range parts {
		if got, want := strings.TrimSpace(p), extCols[i]; got != want {
			t.Fatalf("第 %d 列不一致：cols=%q extCols=%q", i+1, got, want)
		}
	}
}
