package api

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// A WebSocket upgrade takes over the raw connection, which requires the ResponseWriter it is
// handed to implement http.Hijacker. Embedding http.ResponseWriter in a wrapper promotes its
// methods but NOT the optional interfaces the concrete server type also satisfies — so logging
// middleware silently stripped that ability, and /watch failed at the upgrade with "connection
// does not support hijacking".
//
// It failed only in the deployed server: every unit test mounts the handler directly, with no
// middleware between it and the real writer, so the whole watch feature could pass its suite and
// still be unusable. This asserts the property those tests cannot see.
func TestLoggingMiddlewarePreservesConnectionHijacking(t *testing.T) {
	var innerCanHijack bool
	handler := LoggingMiddleware(zap.NewNop())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, innerCanHijack = w.(http.Hijacker)
	}))

	handler.ServeHTTP(hijackableRecorder{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/watch", nil))

	if !innerCanHijack {
		t.Fatal("the handler behind logging middleware cannot hijack the connection — a WebSocket upgrade fails at the handshake")
	}
}

// httptest.ResponseRecorder is not an http.Hijacker, so without this the test would assert that
// the wrapper reports what it wraps rather than that it forwards the capability.
type hijackableRecorder struct{ *httptest.ResponseRecorder }

func (hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }
