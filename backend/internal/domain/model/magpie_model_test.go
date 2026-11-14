// magpie_model_test.go 覆盖 V2 第一期新增列 external_models.magpie_model 的读写与校验口径：
//   - 非空即「可转发」，nil 即「未映射」（NULL 与空串归一，只有一种「未映射」表示）；
//   - 写入前 trim，非法字符/超长直接 400，不静默丢弃；
//   - 两个对外名映射同一代理模型 → 409（唯一索引 uq_external_models_magpie_model 的语义出口）。
package model

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
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
					AddRow(int64(1), "m", "", true, `{"input":1}`, nil, nil, `[]`, c.raw, now, now))
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
