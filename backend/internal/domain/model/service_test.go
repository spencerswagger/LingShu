package model

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/resp"
)

func saleRates() billing.Rates {
	return billing.Rates{"input": 1, "output": 2, "cache_read": 0.1, "cache_write": 0.3, "reasoning": 1}
}

// extCols 与 store.cols 一致，供构造 mock 行。
var extCols = []string{"id", "external_name", "description", "enabled", "sale_rates", "time_config", "context_tiers", "created_at", "updated_at"}

func extRow(m *ExternalModel, saleJSON string) *sqlmock.Rows {
	rows := sqlmock.NewRows(extCols).
		AddRow(m.ID, m.ExternalName, m.Description, m.Enabled, saleJSON, nil, nil, m.CreatedAt, m.UpdatedAt)
	return rows
}

func mockService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(NewStore(db)), mock
}

func TestService_CreateModel(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()

	// GetByName 唯一性检查：无行
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs("qw-max").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(external_name, description, enabled, sale_rates, time_config, context_tiers) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+cols)).
		WithArgs("qw-max", "", true, sqlmock.AnyArg(), nil, nil).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	m, err := s.CreateModel(context.Background(), ExternalModelInput{
		ExternalName: "qw-max", SaleRates: saleRates(),
	})
	if err != nil {
		t.Fatalf("create model: %v", err)
	}
	if m.ID != 1 || m.ExternalName != "qw-max" || !m.Enabled {
		t.Fatalf("unexpected: %+v", m)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_CreateModel_Validation(t *testing.T) {
	s, _ := mockService(t)
	var apiErr *APIError

	// 名称空
	_, err := s.CreateModel(context.Background(), ExternalModelInput{ExternalName: "", SaleRates: saleRates()})
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("期望名称空 40001，got %v", err)
	}
	// 缺键
	ok := saleRates()
	delete(ok, "reasoning")
	_, err = s.CreateModel(context.Background(), ExternalModelInput{ExternalName: "x", SaleRates: ok})
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("期望缺失键 40001，got %v", err)
	}
	// 负值
	neg := saleRates()
	neg["input"] = -1
	_, err = s.CreateModel(context.Background(), ExternalModelInput{ExternalName: "x", SaleRates: neg})
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("期望负值 40001，got %v", err)
	}
}

func TestService_CreateModel_NameExists(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs("qw-max").
		WillReturnRows(extRow(&ExternalModel{ID: 5, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))

	_, err := s.CreateModel(context.Background(), ExternalModelInput{ExternalName: "qw-max", SaleRates: saleRates()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("期望名称冲突 40901，got %v", err)
	}
}

func TestService_UpdateModel(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))
	// external 未变，跳过唯一性预检查
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET external_name=$1, description=$2, enabled=$3, sale_rates=$4, time_config=$5, context_tiers=$6, updated_at=now() WHERE id=$7`)).
		WithArgs("qw-max", "desc", true, sqlmock.AnyArg(), nil, nil, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Description: "desc", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	m, err := s.UpdateModel(context.Background(), 1, ExternalModelInput{
		ExternalName: "qw-max", Description: "desc", Enabled: boolPtr(true), SaleRates: saleRates(),
	})
	if err != nil {
		t.Fatalf("update model: %v", err)
	}
	if m.ExternalName != "qw-max" {
		t.Fatalf("unexpected: %+v", m)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_GetByExternalName(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`)).
		WithArgs("qw-max").
		WillReturnRows(extRow(&ExternalModel{ID: 2, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))

	m, err := s.GetByExternalName(context.Background(), "qw-max")
	if err != nil {
		t.Fatalf("get by external name: %v", err)
	}
	if m.ExternalName != "qw-max" {
		t.Fatalf("unexpected: %+v", m)
	}
}

func TestService_GetByExternalName_NotFound(t *testing.T) {
	s, mock := mockService(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`)).
		WithArgs("nope").
		WillReturnError(sql.ErrNoRows)

	_, err := s.GetByExternalName(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，got %v", err)
	}
}

func TestService_ListModels(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	enabled := true
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE deleted_at IS NULL AND enabled = $1 ORDER BY id ASC`)).
		WithArgs(true).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))
	list, err := s.ListModels(context.Background(), &enabled)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if len(list) != 1 || list[0].ID != 1 {
		t.Fatalf("unexpected: %+v", list)
	}
}

func TestService_BatchDeleteModels(t *testing.T) {
	s, mock := mockService(t)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	n, err := s.BatchDeleteModels(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}
}

type fakePriceSource struct {
	refPrice PriceRef
	has      bool
}

func (f fakePriceSource) ReferencePrice(_ string) (PriceRef, bool) { return f.refPrice, f.has }
func (fakePriceSource) SearchPrices(_ string) []CatalogEntry       { return nil }
func (fakePriceSource) LastUpdated() time.Time                     { return time.Time{} }

func TestService_PriceReferenceNotFound(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))
	s.SetPriceSource(fakePriceSource{has: false})

	_, err := s.PriceReference(context.Background(), 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeNotFound {
		t.Fatalf("期望 models.dev 无该模型价 40401，got %v", err)
	}
}

func TestService_ApplyPrice(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, SaleRates: saleRates(), CreatedAt: now, UpdatedAt: now}, `{"input":1,"output":2,"cache_read":0.1,"cache_write":0.3,"reasoning":1}`))
	s.SetPriceSource(fakePriceSource{refPrice: PriceRef{Input: 0.3, Output: 0.6, UpdatedAt: now}, has: true})
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":0.3,"output":0.6,"cache_read":0.1,"cache_write":0.3,"reasoning":1}`))

	m, err := s.ApplyPrice(context.Background(), 1)
	if err != nil {
		t.Fatalf("apply price: %v", err)
	}
	// scan 只反序列化 sale_rates 键；这里从扫描行取不到具体值，仅验证流程无错且返回模型。
	if m.ExternalName != "qw-max" {
		t.Fatalf("unexpected: %+v", m)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func boolPtr(b bool) *bool { return &b }
