package audit

import (
	"context"
	"regexp"
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
