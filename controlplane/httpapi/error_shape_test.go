package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
)

// shapeSecret is a value no answer may repeat: what a caller sent is never echoed back.
const shapeSecret = "SHAPE-SECRET-7f3a"

type shapeBody struct {
	Name  string `json:"name" maxLength:"8"`
	Count int    `json:"count,omitempty" minimum:"1"`
	Fail  string `json:"fail,omitempty"`
}

// shapeRouter serves one operation that takes a JSON body: fail "rule" refuses it as a rule
// broken on name, and fail "internal" returns an error of the platform's own.
func shapeRouter(t *testing.T) (chi.Router, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	r := authtest.Router(everyKind)
	doc := NewDoc(r, "t", "1.0.0", "", auth.NewAccess(authtest.Placement{}), zap.New(core))
	register(doc, auth.AudienceAgent, huma.Operation{OperationID: "shape", Method: "POST", Path: "/shape"},
		func(_ context.Context, in *struct{ Body shapeBody }) (*struct{}, error) {
			switch in.Body.Fail {
			case "rule":
				return nil, invalid("name", "name is taken")
			case "internal":
				return nil, errors.New("pq: connection refused " + shapeSecret)
			}
			return nil, nil
		})
	return r, logs
}

type shapeAnswer struct {
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Problems []struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Field   string `json:"field"`
	} `json:"problems"`
}

func shapeCall(t *testing.T, r chi.Router, contentType, body string) (int, shapeAnswer, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/shape", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer staff")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, request)
	var answer shapeAnswer
	if recorder.Code != http.StatusNoContent {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: %d %q is not the API's error body: %v", body, recorder.Code, recorder.Body, err)
		}
	}
	return recorder.Code, answer, recorder.Body.String()
}

// 400 is for a request that cannot be read; a request that reads and breaks a rule is 422, each
// problem naming its field and reason. No answer repeats what was sent, nor a parser's words.
func TestARefusalIs400OnlyForWhatCannotBeReadAnd422WithItsFieldsOtherwise(t *testing.T) {
	r, _ := shapeRouter(t)
	for name, tc := range map[string]struct {
		contentType, body string
		status            int
		field             string
	}{
		"not JSON":           {"application/json", `{"name": "` + shapeSecret, http.StatusBadRequest, "body"},
		"empty body":         {"application/json", ``, http.StatusUnprocessableEntity, "body"},
		"wrong type":         {"application/json", `{"name":"ok","count":"` + shapeSecret + `"}`, http.StatusUnprocessableEntity, "body.count"},
		"too long":           {"application/json", `{"name":"` + shapeSecret + `"}`, http.StatusUnprocessableEntity, "body.name"},
		"below its minimum":  {"application/json", `{"name":"ok","count":-7}`, http.StatusUnprocessableEntity, "body.count"},
		"a rule the handler": {"application/json", `{"name":"ok","fail":"rule"}`, http.StatusUnprocessableEntity, "name"},
	} {
		t.Run(name, func(t *testing.T) {
			status, answer, raw := shapeCall(t, r, tc.contentType, tc.body)
			if status != tc.status || answer.Reason != "invalid_request" || answer.Message == "" {
				t.Fatalf("got %d %s, want %d invalid_request with a message", status, raw, tc.status)
			}
			found := false
			for _, p := range answer.Problems {
				found = found || p.Field == tc.field && p.Reason == answer.Reason && p.Message != ""
			}
			if !found {
				t.Errorf("problems %+v, want one on %s with its reason and message", answer.Problems, tc.field)
			}
			for _, leaked := range []string{shapeSecret, "invalid character", "unexpected end", "-7"} {
				if strings.Contains(raw, leaked) {
					t.Errorf("the answer %s repeats %q", raw, leaked)
				}
			}
		})
	}
	if status, _, raw := shapeCall(t, r, "application/json", `{"name":"ok"}`); status != http.StatusNoContent {
		t.Fatalf("a request that keeps every rule: %d %s", status, raw)
	}
}

// A body sent as a type the operation does not take is never read: 415 unsupported_media_type on
// field body, repeating nothing of the body.
func TestABodyOfAnotherContentTypeIsNotRead(t *testing.T) {
	r, _ := shapeRouter(t)
	status, answer, raw := shapeCall(t, r, "text/plain", `{"name":"`+shapeSecret+`"}`)
	if status != http.StatusUnsupportedMediaType || answer.Reason != "unsupported_media_type" || len(answer.Problems) != 1 || answer.Problems[0].Field != "body" || strings.Contains(raw, shapeSecret) {
		t.Fatalf("got %d %s, want 415 unsupported_media_type on body that repeats nothing sent", status, raw)
	}
}

// A failure of the platform's own is answered with the generic body alone and logged once, with
// its operation and the cause.
func TestAFailureIsAnsweredGenericallyAndLoggedOnce(t *testing.T) {
	r, logs := shapeRouter(t)
	status, answer, raw := shapeCall(t, r, "application/json", `{"name":"ok","fail":"internal"}`)
	if status != http.StatusInternalServerError || raw != `{"reason":"internal","message":"internal error"}`+"\n" && raw != `{"reason":"internal","message":"internal error"}` {
		t.Fatalf("got %d %s, want 500 with the generic body", status, raw)
	}
	if len(answer.Problems) != 0 {
		t.Errorf("problems %+v on a failure, want none", answer.Problems)
	}
	entries := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(entries) != 1 {
		t.Fatalf("logged %+v, want the failure once", logs.All())
	}
	fields := entries[0].ContextMap()
	if fields["operation"] != "shape" || !strings.Contains(fmt.Sprint(fields["error"]), "connection refused") {
		t.Errorf("logged %+v, want the operation and the cause", fields)
	}
}
