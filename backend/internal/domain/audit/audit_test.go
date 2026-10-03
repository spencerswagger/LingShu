package audit

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStore_Insert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(
		`INSERT INTO audit_logs(user_id, username, action, target_type, target_id, detail, request_id, ip)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8)`)).
		WithArgs(int64(1), "admin", "admin.user.reset_password", "user", "3", nil, "req-1", "1.2.3.4").
		WillReturnResult(sqlmock.NewResult(1, 1))

	s := NewStore(db)
	if err := s.Insert(context.Background(), Entry{
		UserID: 1, Username: "admin", Action: "admin.user.reset_password",
		TargetType: "user", TargetID: "3", RequestID: "req-1", IP: "1.2.3.4",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// U1：验证审计失败计数器确实有可读出口——计数变化时输出一条含 total/delta 的 Error 日志，
// 未变化时保持安静（避免刷屏）。这钉住"失败从 0 变正数"这一可告警信号真实产生。
func TestReportInsertFailuresOnce_LogsOnChange(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	InsertFailures.Add(1)
	after := InsertFailures.Value()

	got := reportInsertFailuresOnce(logger, 0)
	if got != after {
		t.Fatalf("应返回当前计数 %d，实际 %d", after, got)
	}
	out := buf.String()
	if !strings.Contains(out, "audit insert failures increased") {
		t.Fatalf("计数变化时应输出告警日志，实际: %q", out)
	}
	if !strings.Contains(out, `"total"`) || !strings.Contains(out, `"delta"`) {
		t.Fatalf("告警日志应包含 total/delta 字段: %q", out)
	}

	// 计数未变化：不再输出
	buf.Reset()
	if got2 := reportInsertFailuresOnce(logger, got); got2 != got {
		t.Fatalf("无变化时应保持 last=%d，实际 %d", got, got2)
	}
	if buf.Len() != 0 {
		t.Fatalf("计数未变化时不应输出日志: %q", buf.String())
	}
}
