package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/resp"
)

func saleRates() billing.Rates {
	return billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}
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
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+cols)).
		WithArgs(sqlmock.AnyArg(), "qw-max", "", true, sqlmock.AnyArg(), nil, nil).
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
	q := regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`)

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
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+cols)).
		WithArgs(sqlmock.AnyArg(), "qw-max", "", true, sqlmock.AnyArg(), nil, nil).
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
	rows.AddRow(int64(2), "b", "", true, `{"input":1}`, nil, nil, now, now)
	mock.ExpectQuery(regexp.QuoteMeta(listQ)).WillReturnRows(rows)
	// 第 1 行成功写入。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	// 第 2 行失败 → SyncPrices 提前返回。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(2)).WillReturnError(errors.New("db down"))
	// 预置缓存条目，验证出错路径同样被清空。
	s.store.cacheExternalName("a", nil)

	if _, err := s.SyncPrices(context.Background()); err == nil {
		t.Fatalf("第 2 行 Update 失败时 SyncPrices 应返回错误")
	}
	if fired != 1 {
		t.Fatalf("即便中途出错，也应失效一次（nameWriteHook），got %d", fired)
	}
	s.store.extNameMu.RLock()
	_, present := s.store.extNameCache["a"]
	s.store.extNameMu.RUnlock()
	if present {
		t.Fatalf("出错路径也必须清空对外名缓存（否则旧售价无限期滞留）")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestStore_ExternalNameCacheBounded 锁定 P1-5：对外名读缓存（键来自请求体、用户可控）必须有界，
// 防止认证用户以海量随机名打满负缓存造成内存耗尽 DoS。
func TestStore_ExternalNameCacheBounded(t *testing.T) {
	s := NewStore(nil)
	s.extNameMax = 8
	for i := 0; i < 100; i++ {
		s.cacheExternalName(fmt.Sprintf("m-%d", i), nil) // 负缓存同样计入上限
	}
	s.extNameMu.RLock()
	got := len(s.extNameCache)
	s.extNameMu.RUnlock()
	if got > 8 {
		t.Fatalf("extNameCache 应有界（<=8），got %d", got)
	}
}

// TestStore_GetByExternalName_DefensiveCopy 锁定 P2-7：缓存命中返回防御性拷贝，
// 调用方就地修改返回值（尤其 SaleRates map / 字段）不得污染缓存快照。
func TestStore_GetByExternalName_DefensiveCopy(t *testing.T) {
	s, mock := mockService(t)
	now := time.Now()
	q := regexp.QuoteMeta(`SELECT ` + cols + ` FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`)
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
// 不得使 extNameOrder 无界增长（每次失效须同步从 FIFO 顺序表摘除该名，保持与映射一致）。
func TestStore_InvalidateExternalName_KeepsOrderBounded(t *testing.T) {
	s := NewStore(nil)
	s.extNameMax = 8
	for i := 0; i < 1000; i++ {
		s.cacheExternalName("same", &ExternalModel{ExternalName: "same", Enabled: true})
		s.invalidateExternalName("same")
	}
	s.extNameMu.RLock()
	cacheLen := len(s.extNameCache)
	orderLen := len(s.extNameOrder)
	s.extNameMu.RUnlock()
	if cacheLen != 0 {
		t.Fatalf("失效后缓存应为空，got %d", cacheLen)
	}
	if orderLen != 0 {
		t.Fatalf("extNameOrder 不得无界增长（应与缓存一致为空），got %d", orderLen)
	}
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
	s.cacheExternalName("qw-max", seeded)

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
