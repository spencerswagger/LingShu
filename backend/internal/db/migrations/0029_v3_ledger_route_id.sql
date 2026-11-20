-- 0029_v3_ledger_route_id：账本行携带 magpie 网关的路由 id。
--
-- 背景：请求经 magpie 路由组转发时，账本行的 req_model 是组 ref（group/<id> 或 auto/<model>），
-- served_model 是组内成员模型。灵枢侧售价挂在**对外模型**上，而对外模型的绑定真源是
-- external_models.bound_internal_ref（= 组 ref）。要把「按组路由的账」算到组售价上，
-- 需要知道这行账当时命中的是哪个路由组。
--
-- route_id 的语义（magpie internal/usage usage.go Record.RouteID，已核实）：
--   网关为每个请求分配的数字路由 id（同一路由的多次 try 共享；handed_on 行共享同值）。
--   session 文件来源（origin="session"）与 reported 行没有 route_id（0）。
--   route_id → 组 ref 的映射只在网关侧 routing trace 里，本地镜像不到 →
--   结算侧对 route_id>0 且查不到组的行**必须走回退链**（resolve.go），不许报错堵批次。
--
-- 只加列：BIGINT NOT NULL DEFAULT 0（0 = 无/未知）；回滚 = 删列（不影响历史行）。

ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS route_id BIGINT NOT NULL DEFAULT 0;
