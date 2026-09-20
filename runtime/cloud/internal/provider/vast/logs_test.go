package vast

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	logsPath   = "/api/v0/instances/request_logs/9001/"
	uploadPath = "/store/instance_logs/abc.log"
)

var requestedAt = time.Date(2026, 10, 4, 20, 43, 36, 0, time.UTC)

// logRequest answers the API half of Logs: the upload will be at uploadPath on this server.
func logRequest(api *fakeAPI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", requestedAt.Format(http.TimeFormat))
		answer(200, `{"success":true,"result_url":"`+api.server.URL+uploadPath+`","msg":"in a few seconds"}`)(w, r)
	}
}

func TestLogsWaitsForTheUploadAndSplitsItIntoLines(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPut, logsPath, logRequest(api))
	api.on(http.MethodGet, uploadPath, inSequence(
		answer(403, `<Error><Code>AccessDenied</Code></Error>`),
		answer(403, `<Error><Code>AccessDenied</Code></Error>`),
		upload(requestedAt.Add(time.Second), "first\r\n\nthird with  spaces \nhl-exec: exit-code=3\n"),
	))
	p := newTestProvider(t, api, Settings{})

	lines, err := p.Logs(context.Background(), "9001", 40)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	want := []string{"first", "", "third with  spaces ", "hl-exec: exit-code=3"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	sent := api.sent(http.MethodPut, logsPath)
	if len(sent) != 1 || !reflect.DeepEqual(sent[0].Body, map[string]any{"tail": "40"}) {
		t.Errorf("log requests = %+v", sent)
	}
	if sent[0].Auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q", sent[0].Auth)
	}
	downloads := api.sent(http.MethodGet, uploadPath)
	if len(downloads) != 3 {
		t.Errorf("downloads = %d, want 3", len(downloads))
	}
	if downloads[0].Auth != "" {
		t.Error("the API key was sent to the log store")
	}
}

func TestLogsSkipsAnUploadLeftByAnEarlierRequest(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPut, logsPath, logRequest(api))
	api.on(http.MethodGet, uploadPath, inSequence(
		upload(requestedAt.Add(-time.Minute), "stale\n"),
		upload(requestedAt.Add(-time.Minute), "stale\n"),
		upload(requestedAt, "fresh\n"),
	))
	p := newTestProvider(t, api, Settings{})

	lines, err := p.Logs(context.Background(), "9001", 10)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !reflect.DeepEqual(lines, []string{"fresh"}) {
		t.Errorf("lines = %q, want the upload made for this request", lines)
	}
}

func TestLogsOfAnInstanceThatNeverStartedAreEmpty(t *testing.T) {
	bodies := map[string]string{
		"no container yet": "Error response from daemon: No such container: C.9001\n",
		"nothing written":  "",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPut, logsPath, logRequest(api))
			api.on(http.MethodGet, uploadPath, upload(requestedAt.Add(time.Second), body))
			p := newTestProvider(t, api, Settings{})

			lines, err := p.Logs(context.Background(), "9001", 10)
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}
			if lines == nil || len(lines) != 0 {
				t.Errorf("lines = %#v, want empty", lines)
			}
		})
	}
}

func TestLogsKeepsACommandLineThatOnlyLooksLikeTheMissingContainerNote(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPut, logsPath, logRequest(api))
	body := "Error response from daemon: No such container: C.1234\n"
	api.on(http.MethodGet, uploadPath, upload(requestedAt.Add(time.Second), body))
	p := newTestProvider(t, api, Settings{})

	lines, err := p.Logs(context.Background(), "9001", 10)
	if err != nil || len(lines) != 1 {
		t.Fatalf("Logs = %q, %v, want the one line about another container", lines, err)
	}
}

func TestLogsGivesUpOnAnUploadThatDoesNotArriveOnItsOwnClock(t *testing.T) {
	stuck := map[string]http.HandlerFunc{
		"nothing at the address": answer(403, `<Error><Code>AccessDenied</Code></Error>`),
		"only an old upload":     upload(requestedAt.Add(-time.Minute), "stale\n"),
	}
	for name, store := range stuck {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPut, logsPath, logRequest(api))
			api.on(http.MethodGet, uploadPath, store)
			p := newTestProvider(t, api, Settings{})
			p.logWait = 100 * time.Millisecond

			asked := time.Now()
			_, err := p.Logs(context.Background(), "9001", 10)
			if err == nil || !strings.Contains(err.Error(), "did not deliver its logs within 100ms") {
				t.Fatalf("err = %v, want one saying the host did not deliver in time", err)
			}
			if waited := time.Since(asked); waited > 2*time.Second {
				t.Errorf("Logs took %s to give up", waited)
			}
		})
	}
}

// A status push reads its instances one after another at a few seconds each.
func TestLogsWaitsNoLongerThanAPushCanAfford(t *testing.T) {
	t.Setenv("VAST_TEST_KEY", testKey)
	p, err := New(Settings{APIKeyEnv: "VAST_TEST_KEY", MaxPricePerHour: 0.5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.logWait <= 0 || p.logWait > 30*time.Second {
		t.Errorf("a Logs call waits up to %s for one host", p.logWait)
	}
}

func TestLogsFailures(t *testing.T) {
	cases := map[string]struct {
		request http.HandlerFunc
		store   http.HandlerFunc
		want    string
	}{
		"gone instance": {
			request: answer(404, `{"error":"invalid_id","msg":"Invalid instance id."}`),
			want:    "HTTP 404",
		},
		"rate limit": {
			request: answer(429, `{"detail":"slow down"}`),
			want:    "HTTP 429",
		},
		"server error": {
			request: answer(500, `oops`),
			want:    "HTTP 500",
		},
		"refusal in a 200": {
			request: answer(200, `{"success":false,"msg":"host unreachable"}`),
			want:    "host unreachable",
		},
		"answer without a date": {
			request: func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Date"] = nil
				answer(200, `{"success":true,"result_url":"http://127.0.0.1:1/x"}`)(w, r)
			},
			want: "Date",
		},
		"store error": {
			store: answer(500, `<Error><Code>InternalError</Code></Error>`),
			want:  "log download: HTTP 500",
		},
		"upload without a timestamp": {
			store: answer(200, "line\n"),
			want:  "Last-Modified",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPut, logsPath, logRequest(api))
			if tc.request != nil {
				api.on(http.MethodPut, logsPath, tc.request)
			}
			if tc.store != nil {
				api.on(http.MethodGet, uploadPath, tc.store)
			}
			p := newTestProvider(t, api, Settings{})

			_, err := p.Logs(context.Background(), "9001", 10)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
			if got := len(api.sent(http.MethodPut, logsPath)); got != 1 {
				t.Errorf("log requests = %d, want 1", got)
			}
		})
	}
}

func TestLogsRefusesATailThatIsNotPositive(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, Settings{})

	if _, err := p.Logs(context.Background(), "9001", 0); err == nil {
		t.Fatal("no error for tail 0")
	}
	if api.total() != 0 {
		t.Errorf("requests = %d, want 0", api.total())
	}
}
