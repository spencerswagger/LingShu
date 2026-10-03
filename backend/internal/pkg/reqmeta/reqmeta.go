// Package reqmeta 在请求上下文里携带审计所需的请求元数据（可信 IP、request id），
// 供 service 层在不依赖 *http.Request 的前提下补齐审计字段。
//
// 独立成包而非放在 server：identity 需要读取元数据，若直接依赖 server 会形成
// server → identity → server 的循环依赖。
package reqmeta

import (
	"context"
	"net/http"

	"github.com/team/llmgateway/internal/pkg/clientip"
	"github.com/team/llmgateway/internal/pkg/resp"
)

type ctxKey struct{}

// Meta 请求元数据。
type Meta struct {
	IP        string
	RequestID string
}

// Middleware 将请求元数据注入上下文。须置于 WithRequestID 之后执行，
// 因为 RequestID 依赖 WithRequestID 先把 id 写入上下文。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := Meta{IP: clientip.From(r), RequestID: resp.RequestID(r)}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, m)))
	})
}

// From 读取请求元数据；缺失时返回零值。
// 元数据仅用于审计增强，不参与鉴权，缺失不应影响业务流程。
func From(ctx context.Context) Meta {
	m, _ := ctx.Value(ctxKey{}).(Meta)
	return m
}
