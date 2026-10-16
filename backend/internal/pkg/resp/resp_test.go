package resp

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 内部 API 的每个错误响应都必须落一条结构化日志（本包是全部内部接口错误的唯一出口）。
func TestErrLogsAPIError(t *testing.T) {
	var buf bytes.Buffer
	SetErrorLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { SetErrorLogger(nil) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	req = WithRequestID(req, "req-test-1")
	rec := httptest.NewRecorder()
	Err(rec, req, http.StatusForbidden, CodeForbidden, "无权访问该资源")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	out := buf.String()
	for _, want := range []string{
		"api error",
		"method=GET",
		"path=/api/v1/admin/users",
		"status=403",
		"code=40301",
		"requestId=req-test-1",
		"无权访问该资源",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("异常日志缺少 %q\n实际日志：\n%s", want, out)
		}
	}
	// 响应体仍是内部统一壳，未被日志改动。
	if !strings.Contains(rec.Body.String(), `"Code":40301`) {
		t.Errorf("响应体异常: %s", rec.Body.String())
	}
}

// 未注入日志落点时只写响应，不得 panic（单测/未装配场景安全）。
func TestErrWithoutLoggerDoesNotPanic(t *testing.T) {
	SetErrorLogger(nil)
	rec := httptest.NewRecorder()
	Err(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels", nil),
		http.StatusInternalServerError, CodeInternalError, "服务器内部错误")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}
