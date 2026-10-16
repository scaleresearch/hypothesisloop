package api

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
)

// CORSMiddleware lets one browser origin, the UI's, call the API.
func CORSMiddleware(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return corsHandler(origin, next) }
}

func corsHandler(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
		// Without this a browser cannot read X-Total-Count, so paginated views silently see only
		// the current page's length as the total.
		w.Header().Set("Access-Control-Expose-Headers", "X-Total-Count")

		// Respond immediately to preflight requests.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// LoggingMiddleware returns a middleware that logs method, path, status code,
// and request duration at INFO level.
func LoggingMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rw, r)

			logger.Info("http request",
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Int("status", rw.status),
				zap.Duration("duration", time.Since(start)),
				zap.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// RecoveryMiddleware returns a middleware that catches panics, logs them with a
// stack trace, and responds with HTTP 500. http.ErrAbortHandler is a handler cutting its own
// response on purpose, as a cut stream does: it goes on to the server, which drops the
// connection without logging it, and nothing is written after what was already sent.
func RecoveryMiddleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					auth.WriteError(w, logger, auth.Internal(logger, "panic recovered", fmt.Errorf("%v", rec),
						zap.String("stack", string(debug.Stack())),
						zap.String("method", r.Method),
						zap.String("path", r.URL.Path),
					))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder wraps http.ResponseWriter to capture the written status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the server's writer, to flush a stream and move its
// deadlines.
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// Hijack forwards to the wrapped writer so a WebSocket upgrade still works behind this
// middleware. Embedding http.ResponseWriter promotes its methods but NOT the optional interfaces
// the concrete server type also satisfies, so wrapping silently removes the ability to take over
// the connection -- and the failure surfaces only at the upgrade, as "connection does not support
// hijacking", in the deployed server and never in a test that mounts the handler directly.
func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := sr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("api: underlying %T cannot hijack the connection", sr.ResponseWriter)
	}
	return hijacker.Hijack()
}
