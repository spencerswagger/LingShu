package console

// cost_pricing_golden_test.go 定价曲线的字节级回归。
//
// ⚠️ 口径已变（0024 删掉 channels / channel_keys / channel_models 之后）：
// **成本线不再有配置源**（cost_rates 只存在于 channel_models），所以响应里的 Cost 恒为
// Available=false + 原因文案；渠道维度字段（ChannelID/ChannelName/ChannelModelID）也恒为 0/空。
// 因此「整份响应逐字节不变」已不可能成立，也不该成立。
//
// 现在守的是**售价线**：Sale 子树（费率、时段系数、分档拐点、每个采样点的金额）必须与改动前
// 逐字节一致——这才是这次重构真正动到的风险面（把成本线的数据源拿掉，别把手上的售价算错了）。
// 另加一条显式断言：Cost 必须不可用且带原因，防止有人「顺手」用估算填回去。
//
// golden 文件是在**改动前**的代码上跑同一组场景直接落盘的（见 testdata/*.golden.json），
// 不是手写期望值——所以它能抓住「多加了个字段」「字段顺序变了」「数字格式变了」这类回归，
// 而不仅是「看起来没变」。

import (
	"encoding/json"
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

// assertGoldenSale 只比对 Data.Sale 子树（成本线口径已变，详见文件头）。
func assertGoldenSale(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 golden %s 失败: %v", name, err)
	}
	var g, w struct {
		Data struct {
			Sale json.RawMessage `json:"Sale"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("解析 golden 失败: %v", err)
	}
	if string(g.Data.Sale) != string(w.Data.Sale) {
		t.Fatalf("售价线与改动前不是逐字节一致（%s）\n got: %s\nwant: %s", name, g.Data.Sale, w.Data.Sale)
	}
}

// assertCostRetired 成本线必须是「不可用 + 有原因」，不许被估算值填回去。
func assertCostRetired(t *testing.T, got []byte) {
	t.Helper()
	var g struct {
		Data struct {
			Cost pricingLine `json:"Cost"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if g.Data.Cost.Available {
		t.Fatalf("成本线必须不可用（channel_models 已删，成本取自账本）: %+v", g.Data.Cost)
	}
	if g.Data.Cost.Reason == "" {
		t.Fatalf("不可用的成本线必须给出原因")
	}
	if len(g.Data.Cost.Points) != 0 {
		t.Fatalf("不可用的成本线不许带采样点（否则会被当真实成本展示）: %d 点", len(g.Data.Cost.Points))
	}
}

// TestHandleCostPricingCurve_GoldenUnchanged 逐字节回归：无 DB 无参数、渠道内部模型 + 采样参数、
// 只按对外模型名、非法参数 400 —— 四种场景的响应体都必须与改造前完全相同。
func TestHandleCostPricingCurve_GoldenUnchanged(t *testing.T) {
	t.Run("nil_db_no_params", func(t *testing.T) {
		h := NewAdmin(nil, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertGoldenSale(t, "pricing_curve_nil_db.golden.json", rec.Body.Bytes())
		assertCostRetired(t, rec.Body.Bytes())
	})

	t.Run("sale_line_unchanged", func(t *testing.T) {
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
		// 成本线不再查库（channel_models 已删），只有售价线这一条查询。
		mock.ExpectQuery(regexp.QuoteMeta(`FROM external_models WHERE deleted_at IS NULL AND external_name = $1`)).
			WithArgs("gpt-4o").
			WillReturnRows(sqlmock.NewRows([]string{"id", "external_name", "sale_rates", "time_config", "context_tiers"}).
				AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`), nil, nil))

		h := NewAdmin(db, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/cost/pricing-curve?ExternalModelName=gpt-4o&Step=2000&MaxTokens=40000&HitRate=0.5&InOutRatio=4&At=19:30", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		// 售价线与改动前逐字节一致（golden 取自改动前的代码）。
		assertGoldenSale(t, "pricing_curve_channel_model.golden.json", rec.Body.Bytes())
		assertCostRetired(t, rec.Body.Bytes())
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

		h := NewAdmin(db, nil, nil)
		h.now = func() time.Time { return pricingTestAt(t) }
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/admin/cost/pricing-curve?ExternalModelName=gpt-4o&Step=1000&MaxTokens=10000&At=19:30", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertGoldenSale(t, "pricing_curve_external_name.golden.json", rec.Body.Bytes())
		assertCostRetired(t, rec.Body.Bytes())
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("查询期望未满足: %v", err)
		}
	})

	t.Run("bad_param", func(t *testing.T) {
		h := NewAdmin(nil, nil, nil)
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
	h := NewAdmin(nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve", nil))
	if contains(rec.Body.String(), `"Lines"`) {
		t.Fatalf("未传 ChannelModelIDs 时不应出现 Lines 键: %s", rec.Body.String())
	}
}
