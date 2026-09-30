-- 积分流水新增「覆盖(set)」类型：重建 type 检查约束。
ALTER TABLE credit_flows DROP CONSTRAINT IF EXISTS credit_flows_type_check;
ALTER TABLE credit_flows ADD CONSTRAINT credit_flows_type_check
    CHECK (type IN ('recharge','consume','adjust','set'));
