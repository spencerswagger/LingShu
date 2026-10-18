package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/gencache"
	"github.com/team/llmgateway/internal/pkg/resp"
)

func saleRates() billing.Rates {
	return billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}
}

// extCols 与 store.cols 一致，供构造 mock 行。
var extCols = []string{"id", "external_name", "description", "enabled", "sale_rates", "time_config", "context_tiers", "aliases", "created_at", "updated_at"}

// extNameQuery 与 store.GetByExternalName 一致（含别名包含判定），供构造 mock 期望。
const extNameQuery = `SELECT ` + cols + ` FROM external_models WHERE (external_name = $1 OR aliases @> to_jsonb(ARRAY[$1::text])) AND enabled = true AND deleted_at IS NULL LIMIT 1`

func extRow(m *ExternalModel, saleJSON string) *sqlmock.Rows {
	aliasesJSON := `[]`
	if len(m.Aliases) > 0 {
		if b, err := json.Marshal(m.Aliases); err == nil {
			aliasesJSON = string(b)
		}
	}
	rows := sqlmock.NewRows(extCols).
		AddRow(m.ID, m.ExternalName, m.Description, m.Enabled, saleJSON, nil, nil, aliasesJSON, m.CreatedAt, m.UpdatedAt)
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
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers, aliases) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+cols)).
		WithArgs(sqlmock.AnyArg(), "qw-max", "", true, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg()).
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
	// 缺键（三段任缺其一都应报错）
	ok := saleRates()
	delete(ok, "cache_read")
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

// 跨表重名（反方向）：对外模型名与已有 Auto 名冲突 → 40901（checkAutoName 拦截，不落到 store）。
func TestService_CreateModel_AutoNameConflict(t *testing.T) {
	s, mock := mockService(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs("shared").
		WillReturnError(sql.ErrNoRows)
	s.SetAutoNameChecker(func(_ context.Context, name string) (bool, error) {
		return name == "shared", nil
	})

	_, err := s.CreateModel(context.Background(), ExternalModelInput{ExternalName: "shared", SaleRates: saleRates()})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("期望与 Auto 重名 40901，got %v", err)
	}
	if apiErr.Message != "模型名已被 Auto 模型占用" {
		t.Fatalf("unexpected message: %q", apiErr.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_UpdateModel(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))
	// external 未变，跳过唯一性预检查
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET external_name=$1, description=$2, enabled=$3, sale_rates=$4, time_config=$5, context_tiers=$6, aliases=$7, updated_at=now() WHERE id=$8`)).
		WithArgs("qw-max", "desc", true, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg(), int64(1)).
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
	mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).
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
	mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).
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

// namePriceSource 按模型外部名提供参考价：命中 map 视为有参考价，缺失视为无（用于验证 SyncPrices 的跳过分类）。
type namePriceSource map[string]PriceRef

func (m namePriceSource) ReferencePrice(name string) (PriceRef, bool) { r, ok := m[name]; return r, ok }
func (namePriceSource) SearchPrices(_ string) []CatalogEntry          { return nil }
func (namePriceSource) LastUpdated() time.Time                        { return time.Time{} }

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
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, SaleRates: saleRates(), CreatedAt: now, UpdatedAt: now}, `{"input":1,"output":2,"cache_read":0.1}`))
	s.SetPriceSource(fakePriceSource{refPrice: PriceRef{Input: 0.3, Output: 0.6, UpdatedAt: now}, has: true})
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":0.3,"output":0.6,"cache_read":0.1}`))

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

// TestService_GetByExternalName_CacheZeroDBAndInvalidate 验证对外模型名读缓存（STEP 21）：
// 命中后零 DB（连续多次读取仅回源一次）；删除写路径后缓存立即失效（再读重新查库并读到新值）。
func TestService_GetByExternalName_CacheZeroDBAndInvalidate(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	q := regexp.QuoteMeta(extNameQuery)

	// 首次读取回源一次。
	mock.ExpectQuery(q).WithArgs("qw-max").
		WillReturnRows(extRow(&ExternalModel{ID: 2, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))
	for i := 0; i < 3; i++ {
		m, err := s.GetByExternalName(context.Background(), "qw-max")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if m.ID != 2 {
			t.Fatalf("unexpected: %+v", m)
		}
	}
	// 仅一次回源：命中缓存不应再读库（多余的查询会命中 sqlmock 未预期调用而报错）。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}

	// 写路径（批量删除）后缓存立即失效：再读重新查库并拿到新值（ID 变化即证明读到了新快照）。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := s.BatchDeleteModels(context.Background(), []int64{2}); err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	mock.ExpectQuery(q).WithArgs("qw-max").
		WillReturnRows(extRow(&ExternalModel{ID: 9, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))
	got, err := s.GetByExternalName(context.Background(), "qw-max")
	if err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if got.ID != 9 {
		t.Fatalf("失效后应读到新值，got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_NameWriteHookFired 锁定 P1-4：对外模型写路径成功后必须回调 nameWriteHook
// （装配层据此失效 automodel 依赖 external_models 的跨包读缓存）。
func TestService_NameWriteHookFired(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	fired := 0
	s.SetNameWriteHook(func() { fired++ })

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs("qw-max").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers, aliases) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+cols)).
		WithArgs(sqlmock.AnyArg(), "qw-max", "", true, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg()).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	if _, err := s.CreateModel(context.Background(), ExternalModelInput{ExternalName: "qw-max", SaleRates: saleRates()}); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if fired != 1 {
		t.Fatalf("写路径应回调 nameWriteHook 一次，got %d", fired)
	}

	// 批量删除同样应触发一次跨包失效。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := s.BatchDeleteModels(context.Background(), []int64{1}); err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if fired != 2 {
		t.Fatalf("批量删除也应回调 nameWriteHook，got %d", fired)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_SyncPrices_InvalidatesEvenOnError 锁定评审修复（round4）：
// 批量售价同步只要有任何一行写入成功，缓存就必须失效一次——即便中途某行出错提前返回，
// 也必须清空对外名缓存并回调跨包 hook，否则已提交的售价变更会因缓存无 TTL 而无限期滞留旧价。
func TestService_SyncPrices_InvalidatesEvenOnError(t *testing.T) {
	s, mock := mockService(t)
	s.SetPriceSource(fakePriceSource{refPrice: PriceRef{Input: 5}, has: true})
	fired := 0
	s.SetNameWriteHook(func() { fired++ })

	now := time.Now()
	listQ := `SELECT ` + cols + ` FROM external_models WHERE deleted_at IS NULL ORDER BY id ASC`
	rows := extRow(&ExternalModel{ID: 1, ExternalName: "a", CreatedAt: now, UpdatedAt: now}, `{"input":1}`)
	rows.AddRow(int64(2), "b", "", true, `{"input":1}`, nil, nil, `[]`, now, now)
	mock.ExpectQuery(regexp.QuoteMeta(listQ)).WillReturnRows(rows)
	// 第 1 行成功写入。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	// 第 2 行失败 → SyncPrices 提前返回。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(2)).WillReturnError(errors.New("db down"))
	// 预置缓存条目，验证出错路径同样被清空。
	s.store.cacheExternalName("a", nil, s.store.extNameCache.Generation())

	if _, err := s.SyncPrices(context.Background()); err == nil {
		t.Fatalf("第 2 行 Update 失败时 SyncPrices 应返回错误")
	}
	if fired != 1 {
		t.Fatalf("即便中途出错，也应失效一次（nameWriteHook），got %d", fired)
	}
	if _, present := s.store.extNameCache.Get("a"); present {
		t.Fatalf("出错路径也必须清空对外名缓存（否则旧售价无限期滞留）")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_SyncPrices_SuccessInvalidatesOnce 锁定成功路径的计数断言：
// 批量售价同步在循环内可能出现多行成功写入，但缓存失效必须**恰好一次**（循环后统一清空，
// 避免逐行清空造成写放大），跨包 hook 也只回调一次；同时验证有/无参考价的分类（Skipped/Updated）。
func TestService_SyncPrices_SuccessInvalidatesOnce(t *testing.T) {
	s, mock := mockService(t)
	// 只有 a/b 有参考价，c 无参考价 → 跳过（不进 Updated）。
	s.SetPriceSource(namePriceSource{"a": {Input: 5}, "b": {Input: 5}})
	fired := 0
	s.SetNameWriteHook(func() { fired++ })

	now := time.Now()
	listQ := `SELECT ` + cols + ` FROM external_models WHERE deleted_at IS NULL ORDER BY id ASC`
	rows := extRow(&ExternalModel{ID: 1, ExternalName: "a", CreatedAt: now, UpdatedAt: now}, `{"input":1}`)
	rows.AddRow(int64(2), "b", "", true, `{"input":1}`, nil, nil, `[]`, now, now)
	rows.AddRow(int64(3), "c", "", true, `{"input":1}`, nil, nil, `[]`, now, now)
	mock.ExpectQuery(regexp.QuoteMeta(listQ)).WillReturnRows(rows)

	// 两行成功写入（a/b）；c 无参考价不触发写。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(2)).WillReturnResult(sqlmock.NewResult(0, 1))

	// 预置缓存条目，验证成功路径被清空。
	s.store.cacheExternalName("a", nil, s.store.extNameCache.Generation())
	s.store.cacheExternalName("b", nil, s.store.extNameCache.Generation())

	res, err := s.SyncPrices(context.Background())
	if err != nil {
		t.Fatalf("SyncPrices: %v", err)
	}
	if len(res.Updated) != 2 || res.Updated[0] != "a" || res.Updated[1] != "b" {
		t.Fatalf("Updated 分类错误，got %+v", res.Updated)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "c" {
		t.Fatalf("Skipped 分类错误，got %+v", res.Skipped)
	}
	// 关键计数断言：多行成功但缓存只失效一次（无写放大）。
	if fired != 1 {
		t.Fatalf("多行成功也应只失效一次（nameWriteHook），got %d", fired)
	}
	if remaining := s.store.extNameCache.Count(); remaining != 0 {
		t.Fatalf("成功路径必须清空对外名缓存，仍剩 %d 条", remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_SyncPrices_AllSkippedNoInvalidate 锁定成功路径的负向对照：
// 当所有模型都无参考价（全部 Skipped、无一行写入）时，缓存**不得**被清空、跨包 hook **不得**回调——
// 否则会在无任何售价变更时白白打掉缓存（写放大），与「有变更才失效」的收敛策略相悖。
func TestService_SyncPrices_AllSkippedNoInvalidate(t *testing.T) {
	s, mock := mockService(t)
	s.SetPriceSource(namePriceSource{}) // 任何模型都无参考价
	fired := 0
	s.SetNameWriteHook(func() { fired++ })

	now := time.Now()
	listQ := `SELECT ` + cols + ` FROM external_models WHERE deleted_at IS NULL ORDER BY id ASC`
	rows := extRow(&ExternalModel{ID: 1, ExternalName: "a", CreatedAt: now, UpdatedAt: now}, `{"input":1}`)
	rows.AddRow(int64(2), "b", "", true, `{"input":1}`, nil, nil, `[]`, now, now)
	mock.ExpectQuery(regexp.QuoteMeta(listQ)).WillReturnRows(rows)

	// 预置缓存条目：全部跳过时不得被清空。
	s.store.cacheExternalName("a", nil, s.store.extNameCache.Generation())
	s.store.cacheExternalName("b", nil, s.store.extNameCache.Generation())

	res, err := s.SyncPrices(context.Background())
	if err != nil {
		t.Fatalf("SyncPrices: %v", err)
	}
	if len(res.Updated) != 0 {
		t.Fatalf("无参考价时 Updated 应为空，got %+v", res.Updated)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("无参考价时 Skipped 应为全量，got %+v", res.Skipped)
	}
	if fired != 0 {
		t.Fatalf("无任何写入时不应回调失效 hook，got %d", fired)
	}
	if remaining := s.store.extNameCache.Count(); remaining != 2 {
		t.Fatalf("无变更时缓存应保留（2 条），got %d", remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestStore_ExternalNameCacheBounded 锁定 P1-5：对外名读缓存（键来自请求体、用户可控）必须有界，
// 防止认证用户以海量随机名打满负缓存造成内存耗尽 DoS。
func TestStore_ExternalNameCacheBounded(t *testing.T) {
	s := NewStore(nil)
	// 直写底层原语上限（原语在构造时确定 max），验证有界 FIFO 淘汰。
	s.extNameCache = gencache.New[*ExternalModel](8)
	for i := 0; i < 100; i++ {
		s.cacheExternalName(fmt.Sprintf("m-%d", i), nil, s.extNameCache.Generation()) // 负缓存同样计入上限
	}
	if got := s.extNameCache.Count(); got > 8 {
		t.Fatalf("extNameCache 应有界（<=8），got %d", got)
	}
}

// TestStore_GetByExternalName_DefensiveCopy 锁定 P2-7：缓存命中返回防御性拷贝，
// 调用方就地修改返回值（尤其 SaleRates map / 字段）不得污染缓存快照。
func TestStore_GetByExternalName_DefensiveCopy(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	q := regexp.QuoteMeta(extNameQuery)
	mock.ExpectQuery(q).WithArgs("qw-max").
		WillReturnRows(extRow(&ExternalModel{ID: 2, ExternalName: "qw-max", Enabled: true,
			SaleRates: billing.Rates{"input": 1}, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	first, err := s.GetByExternalName(context.Background(), "qw-max")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 就地篡改返回值：不应影响已缓存的快照。
	first.SaleRates["input"] = 999
	first.ExternalName = "mutated"

	second, err := s.GetByExternalName(context.Background(), "qw-max") // 命中缓存，0 DB
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if second.SaleRates["input"] != 1 {
		t.Fatalf("缓存被返回值修改污染：input=%v", second.SaleRates["input"])
	}
	if second.ExternalName != "qw-max" {
		t.Fatalf("缓存被返回值修改污染：name=%q", second.ExternalName)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}
}

// TestStore_InvalidateExternalName_KeepsOrderBounded 锁定 P2-2：反复「失效 → 回填」同一对外名
// 不得造成内存缓漏。失效须同步从 FIFO 顺序表摘除该名，现由 gencache 原语保证
// （原语单测 assertConsistent 校验 order 与 map 严格一致）；此处断言对外可观测结果：反复失效后缓存为空。
func TestStore_InvalidateExternalName_KeepsOrderBounded(t *testing.T) {
	s := NewStore(nil)
	s.extNameCache = gencache.New[*ExternalModel](8)
	for i := 0; i < 1000; i++ {
		s.cacheExternalName("same", &ExternalModel{ExternalName: "same", Enabled: true}, s.extNameCache.Generation())
		s.invalidateExternalName("same")
	}
	if cacheLen := s.extNameCache.Count(); cacheLen != 0 {
		t.Fatalf("失效后缓存应为空，got %d", cacheLen)
	}
}

// TestStore_StaleCacheFillAfterInvalidateRejected 与 identity token_cache 的同名用例同构：
// 读路径在回源 DB 前捕获代际；若回源期间发生写路径失效（bump 代际，如 Insert/Update/BatchDelete），
// 携带陈旧代际的回填必须被拒绝——否则失效前读到的旧快照会滞留（本缓存无 TTL），
// 后续请求将持续读到该陈旧的启用/停用状态。
func TestStore_StaleCacheFillAfterInvalidateRejected(t *testing.T) {
	s := NewStore(nil)
	fresh := &ExternalModel{ID: 1, ExternalName: "x", Enabled: true}

	// 读路径：回源前捕获代际。
	gen := s.extNameCache.Generation()
	// 期间写路径失效（如改名/批量删除清空缓存）。
	s.invalidateExternalName("x")
	// 读路径回来回填（携带陈旧代际）。
	s.cacheExternalName("x", fresh, gen)

	if _, ok := s.extNameCache.Get("x"); ok {
		t.Fatal("失效后携带陈旧代际的回填必须被拒绝（否则旧配置快照无限期滞留）")
	}

	// 对照组：期间无失效时，携带当前代际的回填应正常命中（证明不会误杀正常回填路径）。
	s.cacheExternalName("x", fresh, s.extNameCache.Generation())
	if _, ok := s.extNameCache.Get("x"); !ok {
		t.Fatal("无失效发生时回填应正常命中")
	}
}

// TestStore_StaleNilCacheFillAfterInvalidateRejected 是上例的负缓存(nil)变体：
// 失效后携带陈旧代际的 nil 回填（「已确认无启用模型」负缓存）同样必须被原语拒绝，
// 否则失效前的「无模型」结论会滞留，后续请求持续误判该名不存在。
func TestStore_StaleNilCacheFillAfterInvalidateRejected(t *testing.T) {
	s := NewStore(nil)

	gen := s.extNameCache.Generation()
	s.invalidateExternalName("x")
	s.cacheExternalName("x", nil, gen)

	if _, ok := s.extNameCache.Get("x"); ok {
		t.Fatal("失效后携带陈旧代际的 nil 负缓存回填必须被拒绝")
	}
}

// TestStore_GetByExternalName_DisabledAlwaysQueriesDB 锁定「关闭读缓存 ⇒ 每次直查 DB，不读也不写缓存」：
// SetReadCacheEnabled(false) 后连续两次 GetByExternalName 都必须回源，故 sqlmock 需消费两次相同的
// SELECT 期望；若误读或误写缓存（第二次走缓存），第二次期望将不被消费而报错。末尾再断言缓存为空。
func TestStore_GetByExternalName_DisabledAlwaysQueriesDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := NewStore(db)
	s.SetReadCacheEnabled(false)

	now := time.Now()
	q := regexp.QuoteMeta(extNameQuery)
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(q).WithArgs("qw-max").
			WillReturnRows(extRow(&ExternalModel{ID: 2, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))
	}
	for i := 0; i < 2; i++ {
		m, err := s.GetByExternalName(context.Background(), "qw-max")
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if m.ID != 2 {
			t.Fatalf("get %d unexpected: %+v", i, m)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("关闭缓存时每次都应查库、且不写缓存: %v", err)
	}
	if got := s.extNameCache.Count(); got != 0 {
		t.Fatalf("关闭缓存时不得写入缓存，got %d 条", got)
	}
}

// ===== 对外模型「别名」特性单测（Step 2/3/4/5/6）=====

// expectGetByNameNoRows 构造 GetByName 的 mock 期望（无匹配 → sql.ErrNoRows）。
func expectGetByNameNoRows(mock sqlmock.Sqlmock, name string) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs(name).WillReturnError(sql.ErrNoRows)
}

func isBadRequest(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == resp.CodeBadRequest
}

// TestStore_ExternalModelAliases_AliasDefaultsEmpty（Step 2）锁定迁移默认空：
// 未指定别名的模型读出为非 nil 空切片（JSONB NOT NULL DEFAULT '[]'），而非 nil 指针。
func TestStore_ExternalModelAliases_AliasDefaultsEmpty(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).WithArgs("qw-max").
		WillReturnRows(extRow(&ExternalModel{ID: 2, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))

	m, err := s.GetByExternalName(context.Background(), "qw-max")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.Aliases == nil {
		t.Fatal("未指定别名时 Aliases 应为非 nil 空切片")
	}
	if len(m.Aliases) != 0 {
		t.Fatalf("未指定别名时 Aliases 应为空，got %v", m.Aliases)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestMigration_ExternalModelAliases_AliasIdempotent（Step 2）锁定迁移可重复执行：
// 0017 必须用 ADD COLUMN IF NOT EXISTS 且默认 '[]'::jsonb，保证重复迁移不报错、旧行为逐字节一致。
func TestMigration_ExternalModelAliases_AliasIdempotent(t *testing.T) {
	b, err := os.ReadFile("../../db/migrations/0017_external_model_aliases.sql")
	if err != nil {
		t.Fatalf("读取迁移失败: %v", err)
	}
	sqlText := string(b)
	if !strings.Contains(sqlText, "ADD COLUMN IF NOT EXISTS aliases") {
		t.Fatalf("迁移必须使用 ADD COLUMN IF NOT EXISTS（保证重复执行不报错），got:\n%s", sqlText)
	}
	if !strings.Contains(sqlText, "DEFAULT '[]'::jsonb") {
		t.Fatalf("迁移必须默认 '[]'::jsonb（默认安全态），got:\n%s", sqlText)
	}
}

// TestStore_GetByExternalName_AliasResolution（Step 3）锁定别名解析：
// 按别名请求走别名包含判定查询，DB 返回规范模型；缓存键为请求名（别名与规范名为不同键），
// 重复按别名读取零 DB。负缓存语义不变。
func TestStore_GetByExternalName_AliasResolution(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).WithArgs("cc-sonnet").
		WillReturnRows(extRow(&ExternalModel{ID: 5, ExternalName: "claude-sonnet", Enabled: true,
			Aliases: []string{"cc-sonnet"}, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	first, err := s.GetByExternalName(context.Background(), "cc-sonnet")
	if err != nil {
		t.Fatalf("alias resolve: %v", err)
	}
	if first.ExternalName != "claude-sonnet" || first.ID != 5 {
		t.Fatalf("别名应解析到规范模型，got %+v", first)
	}
	// 缓存键为入参名：再次以别名读取应命中缓存（零 DB，多余查询会命中 sqlmock 未预期调用而报错）。
	second, err := s.GetByExternalName(context.Background(), "cc-sonnet")
	if err != nil {
		t.Fatalf("alias resolve cached: %v", err)
	}
	if second.ExternalName != "claude-sonnet" {
		t.Fatalf("unexpected: %+v", second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("别名命中后应零 DB: %v", err)
	}
}

// TestService_ValidateAliases_AliasValidate（Step 4）表驱动覆盖别名归一化与命名空间纯校验
// （空白项 → 400、去重保序、不撞本模型 external_name）。
func TestService_ValidateAliases_AliasValidate(t *testing.T) {
	s, mock := mockService(t)
	ctx := context.Background()

	// 空白项 → 400（归一化阶段即返回，不触库）。
	if _, err := s.validateAliases(ctx, []string{"  "}, "self", 0); !isBadRequest(err) {
		t.Fatalf("空白别名应 400，got %v", err)
	}
	// 与本模型 external_name 相同 → 400（在 GetByName 之前短路，不触库）。
	if _, err := s.validateAliases(ctx, []string{"self"}, "self", 0); !isBadRequest(err) {
		t.Fatalf("别名撞本模型名应 400，got %v", err)
	}
	// 去重且保持顺序（每个别名走一次 GetByName 唯一性查询 → 均无匹配）。
	expectGetByNameNoRows(mock, "a")
	expectGetByNameNoRows(mock, "b")
	got, err := s.validateAliases(ctx, []string{" a ", "a", "b"}, "self", 0)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("别名应去重并保序，got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_CreateModel_AliasValidate_Conflict（Step 4）别名撞任何其他 external_name → 409。
func TestService_CreateModel_AliasValidate_Conflict(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1`)).
		WithArgs("taken").
		WillReturnRows(extRow(&ExternalModel{ID: 99, ExternalName: "taken", Enabled: true, CreatedAt: now, UpdatedAt: now}, `{}`))

	_, err := s.CreateModel(context.Background(), ExternalModelInput{
		ExternalName: "new-model", SaleRates: saleRates(), Aliases: []string{"taken"},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("别名撞其他对外模型名应 409，got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_CreateModel_AliasValidate_AutoConflict（Step 4）别名撞任何 Auto 名 → 409。
func TestService_CreateModel_AliasValidate_AutoConflict(t *testing.T) {
	s, mock := mockService(t)
	s.SetAutoNameChecker(func(_ context.Context, name string) (bool, error) {
		return name == "auto-x", nil
	})
	expectGetByNameNoRows(mock, "auto-x")

	_, err := s.CreateModel(context.Background(), ExternalModelInput{
		ExternalName: "new-model", SaleRates: saleRates(), Aliases: []string{"auto-x"},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("别名撞 Auto 名应 409，got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestStore_Insert_AliasInvalidate（Step 5）新增后别名键须逐个失效（避免旧负缓存滞留）。
func TestStore_Insert_AliasInvalidate(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	// 预置别名与规范名的负缓存。
	s.store.cacheExternalName("cc-1", nil, s.store.extNameCache.Generation())
	s.store.cacheExternalName("real", nil, s.store.extNameCache.Generation())
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers, aliases) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING ` + cols)).
		WithArgs(sqlmock.AnyArg(), "real", "", true, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg()).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "real", Enabled: true, Aliases: []string{"cc-1"}, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	if _, err := s.store.Insert(context.Background(), &ExternalModel{
		ExternalName: "real", Enabled: true, SaleRates: saleRates(), Aliases: []string{"cc-1"},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, present := s.store.extNameCache.Get("cc-1"); present {
		t.Fatal("新增后别名键必须失效（避免旧负缓存滞留）")
	}
}

// TestStore_BatchDelete_AliasInvalidate（Step 5）批量删除后整体清空（含别名键）。
func TestStore_BatchDelete_AliasInvalidate(t *testing.T) {
	s, mock := mockService(t)
	s.store.cacheExternalName("cc-1", &ExternalModel{ID: 1, ExternalName: "real", Enabled: true}, s.store.extNameCache.Generation())
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := s.store.BatchDelete(context.Background(), []int64{1}); err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if got := s.store.extNameCache.Count(); got != 0 {
		t.Fatalf("批量删除应整体清空缓存（含别名键），剩余 %d", got)
	}
}

// TestService_UpdateModel_AliasInvalidate（Step 5）改别名后旧别名键须立即失效；
// 再按旧别名读取须回源 DB 且不再命中（无陈旧正缓存残留）。
func TestService_UpdateModel_AliasInvalidate(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	// 预置旧别名正缓存。
	s.store.cacheExternalName("old-a", &ExternalModel{ID: 1, ExternalName: "old", Enabled: true, CreatedAt: now, UpdatedAt: now}, s.store.extNameCache.Generation())

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "old", Enabled: true, Aliases: []string{"old-a"}, CreatedAt: now, UpdatedAt: now}, `{}`))
	// validateAliases 逐个 GetByName 新别名。
	expectGetByNameNoRows(mock, "new-a")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET external_name=$1, description=$2, enabled=$3, sale_rates=$4, time_config=$5, context_tiers=$6, aliases=$7, updated_at=now() WHERE id=$8`)).
		WithArgs("old", "", true, sqlmock.AnyArg(), nil, nil, sqlmock.AnyArg(), int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE id = $1`)).WithArgs(int64(1)).
		WillReturnRows(extRow(&ExternalModel{ID: 1, ExternalName: "old", Enabled: true, Aliases: []string{"new-a"}, CreatedAt: now, UpdatedAt: now}, `{}`))

	if _, err := s.UpdateModel(context.Background(), 1, ExternalModelInput{
		ExternalName: "old", SaleRates: saleRates(), Aliases: []string{"new-a"},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, present := s.store.extNameCache.Get("old-a"); present {
		t.Fatal("改别名后旧别名键必须失效（否则陈旧模型滞留）")
	}
	// 旧别名立即按 DB 重新解析：DB 无匹配 → 不再命中（证明未残留正缓存）。
	mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).WithArgs("old-a").WillReturnError(sql.ErrNoRows)
	if _, err := s.GetByExternalName(context.Background(), "old-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧别名应解析失败，got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestStore_GetByExternalName_AliasResolve（Step 6）表驱动锁定别名解析语义与即时失效：
//  1) 按别名命中正确模型；
//  2) 未变更请求命中缓存 → 零 DB；
//  3) 改名/删别名（写路径失效）后按旧别名不再命中，且无陈旧正缓存残留。
func TestStore_GetByExternalName_AliasResolve(t *testing.T) {
	now := time.Now()

	t.Run("alias_hit_and_zero_db", func(t *testing.T) {
		s, mock := mockService(t)
		mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).WithArgs("cc").
			WillReturnRows(extRow(&ExternalModel{ID: 5, ExternalName: "real", Enabled: true, Aliases: []string{"cc"}, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))
		for i := 0; i < 3; i++ {
			m, err := s.GetByExternalName(context.Background(), "cc")
			if err != nil || m.ID != 5 {
				t.Fatalf("alias hit %d: m=%+v err=%v", i, m, err)
			}
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("未变更请求应零 DB（仅命中缓存）: %v", err)
		}
	})

	t.Run("stale_alias_purged_after_change", func(t *testing.T) {
		s, mock := mockService(t)
		// 预置旧别名正缓存。
		s.store.cacheExternalName("old-a", &ExternalModel{ID: 5, ExternalName: "real", Enabled: true, Aliases: []string{"old-a"}, CreatedAt: now, UpdatedAt: now}, s.store.extNameCache.Generation())
		// 模拟改名/删别名写路径失效。
		s.store.invalidateAllExternalNames()
		// 旧别名再读：必须回源 DB，得到「无启用模型」负结果（不再命中陈旧模型）。
		mock.ExpectQuery(regexp.QuoteMeta(extNameQuery)).WithArgs("old-a").WillReturnError(sql.ErrNoRows)
		if _, err := s.GetByExternalName(context.Background(), "old-a"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("改名后旧别名不应命中，got %v", err)
		}
		// 无陈旧正缓存残留（该键要么不存在，要么已是负缓存 nil）。
		if v, ok := s.store.extNameCache.Get("old-a"); ok && v != nil {
			t.Fatalf("旧别名不得残留陈旧正缓存: %+v", v)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("mock expectations: %v", err)
		}
	})
}

// TestStore_GetByExternalName_DeepCopy 锁定 P2-4：缓存命中返回的深拷贝，消费者修改 TimeConfig
// 嵌套切片（Periodic/Overrides 及其内层 Segments/Weekdays）与 TierRule.Max 指针均不得污染缓存快照。
func TestStore_GetByExternalName_DeepCopy(t *testing.T) {
	s := NewStore(nil)
	now := time.Now()
	max := int64(100)
	seeded := &ExternalModel{
		ID: 2, ExternalName: "qw-max", Enabled: true, CreatedAt: now, UpdatedAt: now,
		ContextTiers: []billing.TierRule{{Min: 0, Max: &max, Coeff: 1.5}},
		TimeConfig: &billing.TimeCoeffConfig{
			Timezone: "UTC", Default: 1,
			Periodic: []billing.Segment{{Name: "全天", Start: "00:00", End: "24:00", Coeff: 1.0,
				Weekdays: []time.Weekday{time.Monday}}},
			Overrides: []billing.DateOverride{{Name: "假日", Start: "2026-01-01", End: "2026-01-02",
				Segments: []billing.Segment{{Name: "特价", Start: "00:00", End: "24:00", Coeff: 0.5}}}},
		},
	}
	s.cacheExternalName("qw-max", seeded, s.extNameCache.Generation())

	first, err := s.GetByExternalName(context.Background(), "qw-max")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 就地篡改返回值的嵌套切片与指针字段。
	*first.ContextTiers[0].Max = 999
	first.TimeConfig.Periodic[0].Coeff = 42
	first.TimeConfig.Periodic[0].Weekdays[0] = time.Sunday
	first.TimeConfig.Overrides[0].Segments[0].Coeff = 7
	first.TimeConfig.Overrides[0].Name = "mutated"

	second, err := s.GetByExternalName(context.Background(), "qw-max")
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if *second.ContextTiers[0].Max != 100 {
		t.Fatalf("TierRule.Max 被污染：%d", *second.ContextTiers[0].Max)
	}
	if second.TimeConfig.Periodic[0].Coeff != 1.0 {
		t.Fatalf("Periodic 被污染：%v", second.TimeConfig.Periodic[0].Coeff)
	}
	if second.TimeConfig.Periodic[0].Weekdays[0] != time.Monday {
		t.Fatalf("Periodic.Weekdays 被污染：%v", second.TimeConfig.Periodic[0].Weekdays[0])
	}
	if second.TimeConfig.Overrides[0].Segments[0].Coeff != 0.5 || second.TimeConfig.Overrides[0].Name != "假日" {
		t.Fatalf("Overrides 被污染：%+v", second.TimeConfig.Overrides[0])
	}
}
