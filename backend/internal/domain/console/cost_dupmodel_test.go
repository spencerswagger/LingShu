package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 这两条用例守「成本口径不会被同名软删模型翻倍」——Owl 复审 P1（真金白银）：
// `uq_external_models_active` 是 `WHERE deleted_at IS NULL` 的**部分**唯一索引，
// 所以「删掉一个对外模型，再用同一个名字建一个」是**合法数据**。成本统计/售价曲线只要
// `LEFT JOIN external_models ON external_name = ...` 而不过滤 `deleted_at`，一行账就会 JOIN 出两行，
// 该实体与本页合计金额直接翻倍（实测 cost=10/sale=12 变成 20/24）。

// TestCostStatsQuery_JoinsOnlyActiveExternalModel 是源码级守卫：成本统计 SQL 的 JOIN 必须带软删过滤。
func TestCostStatsQuery_JoinsOnlyActiveExternalModel(t *testing.T) {
	sql := costStatsQuerySQL
	if !contains(sql, "LEFT JOIN external_models em ON em.external_name = b.external_model_name AND em.deleted_at IS NULL") {
		t.Fatalf("成本统计 JOIN 必须限定 active 行（deleted_at IS NULL），否则同名软删模型会让金额翻倍：%s", sql)
	}
	// 反向：不许出现「没带过滤的同名 JOIN」这种半改法（两个 JOIN 同时存在时前面的会被保留下来）。
	if contains(sql, "ON em.external_name = b.external_model_name\n") {
		t.Fatalf("成本统计里仍有不带 deleted_at 过滤的 JOIN：%s", sql)
	}
}

// TestCostCurvesQuery_JoinsOnlyActiveExternalModel 同口径守售价曲线。
func TestCostCurvesQuery_JoinsOnlyActiveExternalModel(t *testing.T) {
	if !contains(costCurvesQuerySQL, "AND em.deleted_at IS NULL") {
		t.Fatalf("售价曲线必须排除软删模型（同名重建会出两条重复曲线）：%s", costCurvesQuerySQL)
	}
}

// TestCostStats_SoftDeletedDuplicateNameNotDoubled 真库回归：
// 同一个 external_name 先软删再重建，一条账单在成本统计里只能算一次，且 external_model_id 指向**活跃**那行。
func TestCostStats_SoftDeletedDuplicateNameNotDoubled(t *testing.T) {
	d := openCostTempDB(t)

	execCostSQL(t, d, `INSERT INTO users(id, username, password_hash, role) VALUES (8,'dev2','x','DEVELOPER')`)
	// 旧行软删 + 同名新行（部分唯一索引只约束 active 行，这正是「删了再建」合法的地方）。
	execCostSQL(t, d, `INSERT INTO external_models(id, external_name, deleted_at) VALUES (41,'dup-model', now() - interval '1 day')`)
	execCostSQL(t, d, `INSERT INTO external_models(id, external_name) VALUES (42,'dup-model')`)
	execCostSQL(t, d, `INSERT INTO billing_records(id,billing_id,user_id,pricing_mode,external_model_name,internal_model_id,r_value,tokens,credits_consumed,cost_credits,status,call_time) VALUES
	 (11,'d1',8,'sale','dup-model','dup-int',1,'{"input":100,"cache_read":0,"output":10}'::jsonb,12,10,'completed','2026-10-20 10:00:00+08')`)

	h := NewAdmin(d, nil, nil)
	rec := httptest.NewRecorder()
	h.HandleCostStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costStatsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Rows) != 1 {
		t.Fatalf("行数=%d，want 1（同名软删模型不得把一行账拆成两行）: %+v", len(body.Data.Rows), body.Data.Rows)
	}
	row := body.Data.Rows[0]
	if row.CostCredits != 10 || row.SaleCredits != 12 || row.Requests != 1 {
		t.Fatalf("金额被翻倍了（want cost=10/sale=12/req=1）: %+v", row)
	}
	if row.ExternalModelID != 42 {
		t.Fatalf("实体应指向活跃的那行 external_models(id=42)，实际 %d", row.ExternalModelID)
	}
	if body.Data.Total.CostCredits != 10 || body.Data.Total.SaleCredits != 12 {
		t.Fatalf("合计被翻倍了: %+v", body.Data.Total)
	}
}
