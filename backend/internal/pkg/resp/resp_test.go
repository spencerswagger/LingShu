package resp

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/pkg/logger"
)

// 内部 API 的每个错误响应都必须落一条结构化日志（本包是全部内部接口错误的唯一出口）。
// logger 由装配层经 context 注入（无包级全局）。
func TestErrLogsAPIError(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	req = WithRequestID(req, "req-test-1")
	req = req.WithContext(logger.WithLogger(req.Context(), l))

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

// ErrNoLog 用于「能力未启用」这类预期内拒绝：写同样的响应壳，但**不落**异常日志（4xx 告警不误报）。
// 序列化失败（真异常）仍应落 5xx 日志——这里用不可序列化的 Data 走不到，故只验正常路径。
func TestErrNoLogWritesBodyWithoutLogging(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/session/s1/trace", nil)
	req = WithRequestID(req, "req-quiet-1")
	req = req.WithContext(logger.WithLogger(req.Context(), l))

	rec := httptest.NewRecorder()
	ErrNoLog(rec, req, http.StatusForbidden, CodeForbidden, "能力未启用")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if buf.Len() != 0 {
		t.Fatalf("预期内拒绝不应落异常日志，实际：\n%s", buf.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"Code":40301`) || !strings.Contains(body, "能力未启用") ||
		!strings.Contains(body, `"RequestID":"req-quiet-1"`) {
		t.Fatalf("响应体应与 Err 同构（含 Code/Message/RequestID）：%s", body)
	}

	// 对照：Err 同一场景必须落日志（证明差别只在日志，不在响应）。
	rec = httptest.NewRecorder()
	Err(rec, req, http.StatusForbidden, CodeForbidden, "能力未启用")
	if !strings.Contains(buf.String(), "api error") {
		t.Fatalf("Err 应落异常日志，实际：%s", buf.String())
	}
	if rec.Body.String() != body {
		t.Fatalf("ErrNoLog 与 Err 的响应体应完全一致：\nquiet=%s\nlog=%s", body, rec.Body.String())
	}
}

// 未注入 logger（未装配/单测直调）时只写响应，不得 panic。
func TestErrWithoutLoggerDoesNotPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	Err(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels", nil),
		http.StatusInternalServerError, CodeInternalError, "服务器内部错误")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

// 4xx 受客户端错误日志开关控制：关闭后不再落"api error"（5xx 不受影响）。
func TestClientErrorLogSwitch(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })

	base := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	req := base.WithContext(logger.WithLogger(base.Context(), l))

	rec := httptest.NewRecorder()
	Err(rec, req, http.StatusBadRequest, CodeBadRequest, "参数错误")
	if strings.Contains(buf.String(), "api error") {
		t.Errorf("开关关闭时 4xx 不应落日志：%s", buf.String())
	}

	buf.Reset()
	rec = httptest.NewRecorder()
	Err(rec, req, http.StatusInternalServerError, CodeInternalError, "服务器内部错误")
	if !strings.Contains(buf.String(), "api error") {
		t.Errorf("5xx 必须始终落日志：%s", buf.String())
	}
}
