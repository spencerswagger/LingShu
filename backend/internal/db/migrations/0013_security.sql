ALTER TABLE users ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN token_version integer NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_secret_cipher text NULL;
ALTER TABLE users ADD COLUMN totp_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN totp_recovery_hashes jsonb NULL;
ALTER TABLE users ADD COLUMN totp_last_step bigint NOT NULL DEFAULT 0;

CREATE TABLE audit_logs (
  id bigserial PRIMARY KEY,
  user_id bigint NULL,
  -- 审计列用 text：列宽不应成为"能不能被审计"的决定因素
  -- （varchar 超长在 PG 会报错，会让超长用户名/长路径的请求审计静默丢失）。
  username text NULL,
  action text NOT NULL,
  target_type text NULL,
  target_id text NULL,
  detail jsonb NULL,
  request_id varchar(64) NULL,
  ip text NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at DESC);
CREATE INDEX idx_audit_logs_user ON audit_logs(user_id);
