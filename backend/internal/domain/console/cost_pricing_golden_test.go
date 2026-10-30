package console

// cost_pricing_golden_test.go 向后兼容的**硬证据**：不传 ChannelModelIDs 时，
// /cost/pricing-curve 的响应必须与改造前逐字节一致。
//
// golden 文件是在**改动前**的代码上跑同一组场景直接落盘的（见 testdata/*.golden.json），
// 不是手写期望值——所以它能抓住「多加了个字段」「字段顺序变了」「数字格式变了」这类回归，
// 而不仅是「看起来没变」。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 golden %s 失败: %v", name, err)
	}
	if string(got) != string(want) {
		t.Fatalf("响应与改动前不是逐字节一致（%s）\n got: %s\nwant: %s", name, got, want)
	}
}

// TestHandleCostPricingCurve_GoldenUnchanged 逐字节回归：无 DB 无参数、渠道内部模型 + 采样参数、
// 只按对外模型名、非法参数 400 —— 四种场景的响应体都必须与改造前完全相同。
func TestHandleCostPricingCurve_GoldenUnchanged(t *testing.T) {
	t.Run("nil_db_no_params", func(t *testing.T) {
		h := NewAdmin(nil, nil, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertGolden(t, "pricing_curve_nil_db.golden.json", rec.Body.Bytes())
	})

	t.Run("channel_model", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
			WithArgs("billing.time_config").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[
				{"Name":"低谷","Start":"00:00","End":"08:00","Coeff":0.5},
				{"Name":"高峰","Start":"18:00","End":"22:00","Coeff":1.5}]}`)))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
			WithArgs("billing.context_tiers").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`[{"Min":0,"Max":8000,"Coeff":1.0},{"Min":8001,"Max":null,"Coeff":1.5}]`)))
		mock.ExpectQuery(regexp.QuoteMeta(`FROM channel_models cm`)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
				AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int",
					[]byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT cm.external_model_id, em.external_name`)).
			WillReturnRows(sqlmock.NewRows([]string{"external_model_id", "external_name", "sale_rates", "time_config", "context_tiers"}).
				AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`), nil, nil))

		h := NewAdmin(db, nil, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/cost/pricing-curve?ChannelModelID=900&Step=2000&MaxTokens=40000&HitRate=0.5&InOutRatio=4&At=19:30", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertGolden(t, "pricing_curve_channel_model.golden.json", rec.Body.Bytes())
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("查询期望未满足: %v", err)
		}
	})

	t.Run("external_model_name", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		defer db.Close()
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
			WithArgs("billing.time_config").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"Timezone":"Asia/Shanghai","Default":1.0}`)))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
			WithArgs("billing.context_tiers").
			WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`[]`)))
		mock.ExpectQuery(regexp.QuoteMeta(`FROM external_models WHERE deleted_at IS NULL AND external_name = $1`)).
			WithArgs("gpt-4o").
			WillReturnRows(sqlmock.NewRows([]string{"id", "external_name", "sale_rates", "time_config", "context_tiers"}).
				AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`),
					[]byte(`{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[{"Name":"高峰","Start":"18:00","End":"22:00","Coeff":1.5}]}`), nil))

		h := NewAdmin(db, nil, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/cost/pricing-curve?ExternalModelName=gpt-4o&Step=1000&MaxTokens=10000&At=19:30", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertGolden(t, "pricing_curve_external_name.golden.json", rec.Body.Bytes())
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("查询期望未满足: %v", err)
		}
	})

	t.Run("bad_param", func(t *testing.T) {
		h := NewAdmin(nil, nil, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve?Step=abc", nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertGolden(t, "pricing_curve_bad_param.golden.json", rec.Body.Bytes())
	})
}

// TestHandleCostPricingCurve_NoLinesKey 不传 ChannelModelIDs 时响应里根本不出现 Lines 键
// （omitempty 生效；golden 已覆盖字节，这里再给一条可读的断言，失败信息更直白）。
func TestHandleCostPricingCurve_NoLinesKey(t *testing.T) {
	h := NewAdmin(nil, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve", nil))
	if contains(rec.Body.String(), `"Lines"`) {
		t.Fatalf("未传 ChannelModelIDs 时不应出现 Lines 键: %s", rec.Body.String())
	}
}
