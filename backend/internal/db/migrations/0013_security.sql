ALTER TABLE users ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN token_version integer NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_secret_cipher text NULL;
ALTER TABLE users ADD COLUMN totp_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN totp_recovery_hashes jsonb NULL;

CREATE TABLE audit_logs (
  id bigserial PRIMARY KEY,
  user_id bigint NULL,
  username varchar(64) NULL,
  action varchar(64) NOT NULL,
  target_type varchar(32) NULL,
  target_id varchar(64) NULL,
  detail jsonb NULL,
  request_id varchar(64) NULL,
  ip varchar(64) NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_logs_created_at ON audit_logs(created_at DESC);
CREATE INDEX idx_audit_logs_user ON audit_logs(user_id);
