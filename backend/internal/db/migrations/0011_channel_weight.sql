-- 渠道自定义路由权重（同优先级内按权重分配），默认等权 1。
ALTER TABLE channels ADD COLUMN IF NOT EXISTS weight INT NOT NULL DEFAULT 1;
