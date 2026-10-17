package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"go.uber.org/zap"
)

// Tokens resolves a token. *Authenticator is the one used in service.
type Tokens interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// publicPaths answer without a token: liveness, and the documents describing the API, at the root
// and under each mounted API's prefix.
var publicPaths = map[string]bool{
	"/health": true, "/healthz": true,
	"/docs": true, "/openapi.json": true, "/openapi.yaml": true, "/openapi-3.0.json": true, "/openapi-3.0.yaml": true,
	"/explore": true, "/explore/coordinator": true, "/explore/rules": true,
}

var publicPrefixes = []string{clusterapi.ClustersPath}

func public(path string) bool {
	for _, prefix := range publicPrefixes {
		if rest, ok := strings.CutPrefix(path, prefix); ok && strings.HasPrefix(rest, "/") {
			path = rest
			break
		}
	}
	return publicPaths[path] || strings.HasPrefix(path, "/schemas/")
}

// Gate is the one check every request passes before any handler runs: it resolves the request's
// token and hands the handler a context carrying the principal. What the principal may do is
// declared per route and asked of that principal (RequireStaff, RequireRuntime, an
// Audience). webSockets names the paths a WebSocket client opens with the subprotocol
// each speaks: a browser cannot set a header on a WebSocket, so such a client offers its token
// as the second of two subprotocols.
func Gate(tokens Tokens, logger *zap.Logger, webSockets map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if public(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			git := strings.HasPrefix(r.URL.Path, GitPrefix)
			if git {
				// git sends a credential only once a 401 asks for one in a scheme it knows.
				w.Header().Set("WWW-Authenticate", `Basic realm="hypothesisloop"`)
			}
			token, err := requestToken(r, webSockets, git)
			if err != nil {
				WriteError(w, logger, err)
				return
			}
			principal, err := tokens.Authenticate(r.Context(), token)
			if err != nil {
				var refused *refusal.Refusal
				if !errors.As(err, &refused) {
					err = Internal(logger, "could not authenticate a request", err, zap.String("path", r.URL.Path))
				}
				WriteError(w, logger, err)
				return
			}
			if git {
				w.Header().Del("WWW-Authenticate")
			}
			recheck := func(ctx context.Context) (Principal, error) { return tokens.Authenticate(ctx, token) }
			ctx := context.WithValue(WithPrincipal(r.Context(), principal), recheckKey{}, recheck)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// GitPrefix is where the git proxy is served. git's credential helpers answer with a username
// and a password, so a request there may carry its token as the password of Basic
// authentication, beside the Bearer header every request may carry it in.
const GitPrefix = "/git/"

func requestToken(r *http.Request, webSockets map[string]string, git bool) (string, error) {
	if protocol, ok := webSockets[r.URL.Path]; ok {
		return subprotocolToken(r.Header, protocol)
	}
	if _, password, ok := r.BasicAuth(); git && ok {
		if password == "" {
			return "", unauthenticated("send your platform token as the password")
		}
		return password, nil
	}
	return bearerToken(r.Header.Get("Authorization"))
}

func bearerToken(header string) (string, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", unauthenticated("send Authorization: Bearer <token>")
	}
	return token, nil
}

func subprotocolToken(h http.Header, protocol string) (string, error) {
	var offered []string
	for _, value := range h.Values("Sec-WebSocket-Protocol") {
		for _, part := range strings.Split(value, ",") {
			offered = append(offered, strings.TrimSpace(part))
		}
	}
	if len(offered) != 2 || offered[0] != protocol || offered[1] == "" {
		return "", Unauthenticated("send Sec-WebSocket-Protocol: " + protocol + ", <token>")
	}
	return offered[1], nil
}

type recheckKey struct{}

// Recheck resolves again the token the request passed Gate with: a stream outlives the request
// that opened it, and the token may have died since.
func Recheck(ctx context.Context) (Principal, error) {
	recheck, ok := ctx.Value(recheckKey{}).(func(context.Context) (Principal, error))
	if !ok {
		return Principal{}, errNotGated
	}
	return recheck(ctx)
}

var errNotGated = errors.New("the request did not pass auth.Gate")

// RequireRuntime serves next only to a runtime token of a registered cluster, or to staff. It
// guards the plain HTTP endpoints runtimes push to outside the documented API.
func RequireRuntime(logger *zap.Logger, next http.Handler) http.Handler {
	return require(logger, "only a runtime token may push here", next, KindRuntime, KindStaff)
}

// RequireStaff serves next only to staff.
func RequireStaff(logger *zap.Logger, next http.Handler) http.Handler {
	return require(logger, "staff only", next, KindStaff)
}

func require(logger *zap.Logger, refusal string, next http.Handler, kinds ...Kind) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			WriteError(w, logger, Internal(logger, "a guarded route was reached", errNotGated, zap.String("path", r.URL.Path)))
			return
		}
		for _, kind := range kinds {
			if p.Kind == kind {
				next.ServeHTTP(w, r)
				return
			}
		}
		WriteError(w, logger, forbidden(refusal))
	})
}
