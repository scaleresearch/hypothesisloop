package vast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

func TestListReturnsOnlyThePrefixAcrossPages(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, listPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("after_token") {
		case "":
			answer(200, instancesPage("page-2",
				instanceJSON(11, "hl-exp1-a1", "running"),
				instanceJSON(12, "someone-elses", "running"),
				instanceJSON(13, "", "running"),
			))(w, r)
		case "page-2":
			answer(200, instancesPage("",
				instanceJSON(14, "hl-exp2-a1", "loading"),
				instanceJSON(15, "xhl-exp3-a1", "running"),
			))(w, r)
		default:
			t.Errorf("after_token = %q", r.URL.Query().Get("after_token"))
		}
	})
	p := newTestProvider(t, api, Settings{})

	got, err := p.List(context.Background(), "hl-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	started := time.Unix(4102444800, 0)
	want := []provider.Instance{
		{ID: "11", Name: "hl-exp1-a1", State: provider.StateRunning, StartedAt: started},
		{ID: "14", Name: "hl-exp2-a1", State: provider.StateStarting, StartedAt: started},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List\n got %v\nwant %v", got, want)
	}
	pages := api.sent(http.MethodGet, listPath)
	if len(pages) != 2 || pages[0].Query != "limit=25" || pages[1].Query != "after_token=page-2&limit=25" {
		t.Errorf("page requests = %+v", pages)
	}
	if pages[0].Auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q", pages[0].Auth)
	}
}

func TestListReturnsEveryInstanceOfANameTheVendorHoldsTwice(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, listPath, answer(200, instancesPage("",
		instanceJSON(21, "hl-job-ns-exp1-a1", "running"),
		instanceJSON(22, "hl-job-ns-exp1-a1", "loading"),
	)))
	p := newTestProvider(t, api, Settings{})

	got, err := p.List(context.Background(), "hl-job-ns-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != "21" || got[1].ID != "22" || got[0].Name != got[1].Name {
		t.Errorf("List = %v, want both instances of the name", got)
	}
}

func TestListReturnsAMarkedInstanceUnderItsOwnName(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, listPath, answer(200, instancesPage("",
		instanceJSON(31, "hl-job-ns-exp1-a1.stop", "running"),
		instanceJSON(32, "other.stop", "running"),
	)))
	p := newTestProvider(t, api, Settings{})

	for _, prefix := range []string{"hl-job-ns-", "hl-job-ns-exp1-a1"} {
		got, err := p.List(context.Background(), prefix)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != "31" || got[0].Name != "hl-job-ns-exp1-a1" || got[0].State != provider.StateRunning {
			t.Errorf("List(%q) = %v, want the instance under the name it was created with", prefix, got)
		}
	}
}

func TestListOfAnEmptyAccountIsEmpty(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, listPath, answer(200, instancesPage("")))
	p := newTestProvider(t, api, Settings{})

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 0 {
		t.Fatalf("List = %v, %v", got, err)
	}
}

func TestListMapsVendorStatuses(t *testing.T) {
	// "loading" is also where an instance stays when its image cannot be pulled or its host never
	// brings it up: starting, until the caller's own boot timeout ends the wait.
	cases := map[string]provider.State{
		"":           provider.StateStarting,
		"loading":    provider.StateStarting,
		"created":    provider.StateStarting,
		"scheduling": provider.StateStarting,
		"offline":    provider.StateStarting,
		"running":    provider.StateRunning,
		"exited":     provider.StateExited,
	}
	for status, want := range cases {
		t.Run("status "+status, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(1, "hl-a", status))))
			p := newTestProvider(t, api, Settings{})

			got, err := p.List(context.Background(), "hl-")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != 1 || got[0].State != want {
				t.Errorf("List = %v, want state %s", got, want)
			}
		})
	}
}

// One instance in a status the adapter has never seen must not hide the account: it is listed as
// starting, so it keeps its slot and can be destroyed, and the log says what the vendor reported.
func TestListReportsAStatusItDoesNotKnowAsStartingAndLogsIt(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	api := newFakeAPI(t)
	api.on(http.MethodGet, listPath, answer(200, instancesPage("",
		instanceJSON(1, "hl-a", "running"),
		instanceJSON(2, "hl-b", "hibernating"),
		instanceJSON(3, "other", "hibernating"),
		instanceJSON(4, "hl-c", "exited"),
	)))
	p := newTestProvider(t, api, Settings{})

	got, err := p.List(context.Background(), "hl-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []provider.State{provider.StateRunning, provider.StateStarting, provider.StateExited}
	if len(got) != 3 || got[0].State != want[0] || got[1].State != want[1] || got[2].State != want[2] || got[1].Name != "hl-b" || got[1].ID != "2" {
		t.Errorf("List = %v, want all three instances with hl-b as starting", got)
	}
	if line := logged.String(); !strings.Contains(line, "hl-b") || !strings.Contains(line, `"hibernating"`) {
		t.Errorf("log = %q, want the instance name and the raw status", line)
	}
	if strings.Contains(logged.String(), "other") {
		t.Errorf("log = %q, mentions an instance outside the prefix", logged.String())
	}
}

func TestListDoesNotRetryARateLimitOrAServerError(t *testing.T) {
	for _, status := range []int{429, 502} {
		api := newFakeAPI(t)
		api.on(http.MethodGet, listPath, answer(status, `<html>bad gateway</html>`))
		p := newTestProvider(t, api, Settings{})

		_, err := p.List(context.Background(), "hl-")
		if err == nil || !strings.Contains(err.Error(), "HTTP "+jsonInt(status)) {
			t.Fatalf("err = %v, want one carrying HTTP %d", err, status)
		}
		if api.total() != 1 {
			t.Errorf("requests = %d, want 1", api.total())
		}
	}
}

const gone = `{"success":false,"error":"no_such_instance","msg":"Instance 9001 not found."}`

func TestDestroyDeletesTheInstance(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, "/api/v0/instances/9001/", answer(200, `{"success":true}`))
	p := newTestProvider(t, api, Settings{})

	if err := p.Destroy(context.Background(), "9001"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if sent := api.sent(http.MethodDelete, "/api/v0/instances/9001/"); len(sent) != 1 || sent[0].Auth != "Bearer "+testKey {
		t.Errorf("destroy requests = %+v", sent)
	}
}

func TestDestroyOfAGoneInstanceIsNotAnError(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, "/api/v0/instances/9001/", answer(404, gone))
	p := newTestProvider(t, api, Settings{})

	if err := p.Destroy(context.Background(), "9001"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestDestroyReportsEveryOtherFailure(t *testing.T) {
	failures := map[string]http.HandlerFunc{
		"server error":       answer(500, `oops`),
		"another 404":        answer(404, `{"success":false,"error":"not_found","msg":"no such route"}`),
		"refusal in a 200":   answer(200, `{"success":false,"msg":"instance is locked"}`),
		"not ours to change": answer(403, `{"success":false,"error":"not_authorized"}`),
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodDelete, "/api/v0/instances/9001/", failure)
			p := newTestProvider(t, api, Settings{})

			if err := p.Destroy(context.Background(), "9001"); err == nil {
				t.Fatal("no error")
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want 1", api.total())
			}
		})
	}
}

func stuckInstance(id int, label, status, message string, age time.Duration) string {
	encoded, _ := json.Marshal(message)
	return fmt.Sprintf(`{"id":%d,"label":%q,"actual_status":%q,"status_msg":%s,"machine_id":124592,"start_date":%d}`,
		id, label, status, encoded, time.Now().Add(-age).Unix())
}

// An instance that never comes up is ended by the caller's boot timeout, and the host's message
// is the only account of why. The contract has no field for it, so List writes it to the log.
func TestListLogsWhatTheHostSaysAboutAnInstanceThatIsSlowToStart(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	const cdi = "Error response from daemon: failed to inject CDI devices:\n unresolvable CDI devices nvidia.com/gpu=3\n"
	api := newFakeAPI(t)
	api.on(http.MethodGet, listPath, answer(200, instancesPage("",
		stuckInstance(1, "hl-stuck", "created", cdi, 20*time.Minute),
		stuckInstance(2, "hl-pulling", "loading", "e36530ed2a4d: Downloading", 2*time.Minute),
		stuckInstance(3, "hl-running", "running", "success, running img", 20*time.Minute),
		stuckInstance(4, "other-stuck", "created", cdi, 20*time.Minute),
		stuckInstance(5, "hl-silent", "created", "", 20*time.Minute),
		stuckInstance(6, "hl-loud", "created", strings.Repeat("x", 5000), 20*time.Minute),
	)))
	p := newTestProvider(t, api, Settings{})

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 5 || got[0].State != provider.StateStarting {
		t.Fatalf("List = %v, %v", got, err)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("log has %d lines, want one for each of the three slow instances of the prefix:\n%s", len(lines), logged.String())
	}
	for _, want := range []string{"hl-stuck", "machine 124592", "failed to inject CDI devices: unresolvable CDI devices nvidia.com/gpu=3"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line %q lacks %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "hl-silent") || !strings.Contains(lines[1], "no status message") {
		t.Errorf("log line %q, want the silent instance named", lines[1])
	}
	if len(lines[2]) > 600 {
		t.Errorf("a log line of %d bytes carries the host's whole message", len(lines[2]))
	}
}

const tooMany = `{"message": "The action could not be performed because there were too many requests by the client."}`

// Seen on a real account: two destroys of one instance were refused with 429 in a row. An
// instance that is not destroyed is paid for.
func TestDestroyAsksAgainWhenTheAPISaysTheAccountCalledTooOften(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, "/api/v0/instances/9001/", inSequence(answer(429, tooMany), answer(429, tooMany), answer(200, `{"success":true}`)))
	p := newTestProvider(t, api, Settings{})

	if err := p.Destroy(context.Background(), "9001"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if sent := api.sent(http.MethodDelete, "/api/v0/instances/9001/"); len(sent) != 3 {
		t.Fatalf("destroy requests = %d, want the two refused and the one that went through", len(sent))
	}
}

func TestDestroyRefusedUntilTimeRunsOutSaysTheInstanceIsStillRented(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, "/api/v0/instances/9001/", answer(429, tooMany))
	p := newTestProvider(t, api, Settings{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err := p.Destroy(ctx, "9001")
	if err == nil || !strings.Contains(err.Error(), "still rented") || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("Destroy = %v, want it to say the instance is still rented and why", err)
	}
	if sent := api.sent(http.MethodDelete, "/api/v0/instances/9001/"); len(sent) < 2 {
		t.Fatalf("destroy requests = %d, want more than one try", len(sent))
	}
}
