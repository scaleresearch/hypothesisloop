package metricsingest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type placedOn map[string]string

func (p placedOn) PlacementByID(_ context.Context, ids []string) (map[string]domain.Placement, error) {
	out := map[string]domain.Placement{}
	for _, id := range ids {
		if cluster, ok := p[id]; ok {
			out[id] = domain.Placement{ClusterName: cluster}
		}
	}
	return out, nil
}

// metricsDB records the remote-write bodies it is sent, decompressed. A label value sits in the
// protobuf as plain bytes, so a job id is found by looking for it.
func metricsDB(t *testing.T) (*httptest.Server, *[][]byte) {
	t.Helper()
	var writes [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Errorf("decode remote write: %v", err)
		}
		writes = append(writes, decoded)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server, &writes
}

func pushAs(t *testing.T, handler http.Handler, caller *auth.Principal, jobs ...string) int {
	t.Helper()
	payload := PushRequest{Node: "node-1", Timestamp: time.Now().UTC()}
	for _, job := range jobs {
		payload.Pods = append(payload.Pods, PodSample{PodUID: "uid-" + job, ExperimentID: job})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/node-metrics", bytes.NewReader(body))
	if caller != nil {
		request = request.WithContext(auth.WithPrincipal(request.Context(), *caller))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

// A heartbeat keeps a job alive and is what its runtime is billed from, so a runtime writes one
// only for a job placed on its own cluster.
func TestALivenessSampleCountsOnlyForAJobPlacedOnTheCallersCluster(t *testing.T) {
	placements := placedOn{"job-mine": "cluster-a", "job-also-mine": "cluster-a", "job-theirs": "cluster-b", "job-unplaced": ""}
	runtimeA := auth.Principal{Kind: auth.KindRuntime, ClusterName: "cluster-a", OrgIDs: []string{"org-a"}}
	for name, tc := range map[string]struct {
		caller  *auth.Principal
		jobs    []string
		status  int
		written []string
		dropped []string
	}{
		"own jobs":                          {&runtimeA, []string{"job-mine", "job-also-mine"}, 204, []string{"job-mine", "job-also-mine"}, nil},
		"another cluster's job":             {&runtimeA, []string{"job-theirs"}, 204, nil, []string{"job-theirs"}},
		"own and another's":                 {&runtimeA, []string{"job-theirs", "job-mine"}, 204, []string{"job-mine"}, []string{"job-theirs"}},
		"a job placed nowhere":              {&runtimeA, []string{"job-unplaced"}, 204, nil, []string{"job-unplaced"}},
		"a job that does not exist":         {&runtimeA, []string{"job-missing"}, 204, nil, []string{"job-missing"}},
		"a runtime token naming no cluster": {&auth.Principal{Kind: auth.KindRuntime}, []string{"job-unplaced"}, 204, nil, []string{"job-unplaced"}},
		"staff":                             {&auth.Principal{Kind: auth.KindStaff}, []string{"job-theirs"}, 204, []string{"job-theirs"}, nil},
		"no principal":                      {nil, []string{"job-mine"}, 401, nil, []string{"job-mine"}},
	} {
		db, writes := metricsDB(t)
		code := pushAs(t, NewPushHandler(db.URL, placements), tc.caller, tc.jobs...)
		if code != tc.status {
			t.Errorf("%s: status = %d, want %d", name, code, tc.status)
		}
		all := bytes.Join(*writes, nil)
		for _, job := range tc.written {
			if !bytes.Contains(all, []byte(job)) {
				t.Errorf("%s: no heartbeat was written for %s", name, job)
			}
		}
		for _, job := range tc.dropped {
			if bytes.Contains(all, []byte(job)) {
				t.Errorf("%s: a heartbeat was written for %s", name, job)
			}
		}
		if len(tc.written) == 0 && len(*writes) != 0 {
			t.Errorf("%s: %d writes reached the metrics database, want none", name, len(*writes))
		}
	}
}
