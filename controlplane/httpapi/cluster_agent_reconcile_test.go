package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// reconcileStore is what two clusters hold: the jobs placed on each, the jobs each just lost,
// and the identity each is bound to.
type reconcileStore struct {
	clusterexchange.Store
	desired   map[string][]*domain.Experiment
	undesired map[string][]*domain.Experiment
	boundTo   map[string]string
	bindErr   error
	binds     []string
	// runReportedAt is when the end of each job's run was first reported, and runsAsked the jobs
	// each status push asked it of.
	runReportedAt map[string]time.Time
	runsAsked     [][]string
	// refusals is what each reconcile was refused with, empty for one accepted, as
	// RecordContactRefusal kept it.
	refusals []string
}

func (s *reconcileStore) RunFirstReportedAt(_ context.Context, ids []string) (map[string]time.Time, error) {
	s.runsAsked = append(s.runsAsked, ids)
	return s.runReportedAt, nil
}

func (s *reconcileStore) ListDesiredWorkloads(_ context.Context, cluster string) ([]*domain.Experiment, error) {
	return s.desired[cluster], nil
}

func (s *reconcileStore) ListRecentlyUndesiredWorkloads(_ context.Context, cluster string, _ time.Duration) ([]*domain.Experiment, error) {
	return s.undesired[cluster], nil
}

func (s *reconcileStore) BindReportedID(_ context.Context, name, id string) error {
	s.binds = append(s.binds, name+"="+id)
	if s.bindErr != nil {
		return s.bindErr
	}
	s.boundTo[name] = id
	return nil
}

type reconcileWorld struct {
	store  *reconcileStore
	router http.Handler
	// metricsWrites counts the series written, and sqlDown fails the statements that write a
	// job's log tail and phase detail.
	metricsWrites atomic.Int32
	sqlDown       atomic.Bool
}

func newReconcileWorld(t *testing.T, cluster string) *reconcileWorld {
	t.Helper()
	return newReconcileWorldWith(t, cluster, func(base *reconcileStore) clusterexchange.Store { return base })
}

// newReconcileWorldWith serves the handler over the store wrap makes of the world's own.
func newReconcileWorldWith(t *testing.T, cluster string, wrap func(*reconcileStore) clusterexchange.Store) *reconcileWorld {
	t.Helper()
	w := &reconcileWorld{store: &reconcileStore{desired: map[string][]*domain.Experiment{}, undesired: map[string][]*domain.Experiment{}, boundTo: map[string]string{}}}
	metrics := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sql" {
			answer := `{"output":[{"affectedrows":1}]}`
			if w.sqlDown.Load() {
				answer = `{"code":1003,"error":"the store is down"}`
			}
			_, _ = rw.Write([]byte(answer))
			return
		}
		w.metricsWrites.Add(1)
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(metrics.Close)
	data := &objectstore.Client{Endpoint: "http://store.invalid:9000", Region: "us-east-1", Bucket: "data"}
	caller := authtest.As{Kind: auth.KindRuntime, ClusterName: cluster, OrgIDs: []string{"org-a"}}
	router := authtest.Router(caller)
	doc := NewDoc(router, "test", "1.0.0", "", auth.NewAccess(authtest.Placement{Clusters: map[string]string{cluster: "org-a"}}), zap.NewNop())
	RegisterClusterAgent(doc, clusterexchange.NewService(clusterexchange.Config{
		Store: wrap(w.store), ConnectedWithin: time.Minute, Metrics: metricsdb.New(metrics.URL), ObservationCadence: 90 * time.Second, DataStore: data,
		MaxCheckpointGrace: time.Minute, MaxLogLineChars: 100, SigningSecret: "test-signing-secret-of-enough-length", ProxyURL: "https://api.test/git", Logger: zap.NewNop(),
	}))
	w.router = router
	return w
}

func idleReport(clusterID string) map[string]any {
	node := map[string]int64{domain.NodeResourceCPUMillicores: 1000, domain.NodeResourceMemoryBytes: 1 << 30, domain.NodeResourceStorageBytes: 1 << 30}
	return map[string]any{
		"cluster_id":          clusterID,
		"cpu_available_cores": 1, "cpu_total_cores": 1,
		"ram_available_bytes": 1 << 30, "ram_total_bytes": 1 << 30,
		"storage_available_bytes": 1 << 30, "storage_total_bytes": 1 << 30,
		"accelerator_available_by_type": map[string]int64{}, "accelerator_total_by_type": map[string]int64{},
		"accelerator_available_by_node": map[string]any{},
		"node_resources_by_node":        map[string]any{"node-1": node},
		"node_resources_total_by_node":  map[string]any{"node-1": node},
		"node_labels_by_node":           map[string]any{"node-1": map[string]string{}},
		"multi_node_capable":            false, "autoscaler_enabled": false,
	}
}

func (w *reconcileWorld) reconcile(t *testing.T, cluster string, report map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	req := authtest.Authorize(httptest.NewRequest("POST", "/"+cluster+"/reconcile", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	return rec
}

func placedJob(id, org string) *domain.Experiment {
	return &domain.Experiment{
		Image: "registry.test/image@sha256:aa", ID: id, AgentID: "agent-" + id, OrgID: org, PlatformExperimentID: "pe-" + id, ProjectID: "prj-" + id,
		HypothesisID: "hyp-" + id, Hypothesis: "the hypothesis of " + org, Theory: "the theory of " + org,
		ClusterName: "of-a", Status: domain.StatusRunning, AttemptCount: 2,
		Job:     domain.JobSpec{},
		DataKey: domain.DataKey{AccessKeyID: "AK-" + id, SecretAccessKey: "SK-" + id},
	}
}

// What a cluster's runtime is answered: the jobs placed on it, each cut to what a runtime builds a
// workload from, and a checkpoint window only for jobs that ran on it.
func TestReconcileAnswersAClusterWithItsOwnJobsAndNothingElseOfThem(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.desired["of-a"] = []*domain.Experiment{placedJob("job-own", "org-a"), placedJob("job-guest", "org-b")}
	w.store.desired["of-b"] = []*domain.Experiment{placedJob("job-elsewhere", "org-b")}
	preempted := func(id string) *domain.Experiment {
		return &domain.Experiment{ID: id, Status: domain.StatusQueued, EvictionReason: string(domain.EvictionPreemptedForGuaranteed)}
	}
	w.store.undesired["of-a"] = []*domain.Experiment{preempted("ran-here"), {ID: "crashed-here", Status: domain.StatusFailed, EvictionReason: string(domain.EvictionWorkloadGone)}}
	w.store.undesired["of-b"] = []*domain.Experiment{preempted("ran-on-another-orgs-cluster")}

	rec := w.reconcile(t, "of-a", idleReport("id-of-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	var answer struct {
		Experiments      []map[string]any `json:"experiments"`
		CheckpointWindow []string         `json:"checkpoint_window"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	handed := []string{}
	for _, exp := range answer.Experiments {
		handed = append(handed, exp["id"].(string))
		if exp["token"] == "" || exp["token"] == nil || exp["data"] == nil || exp["agent_id"] == "" || exp["attempt_count"] != float64(2) {
			t.Errorf("job %v is handed without what its workload is built from: %v", exp["id"], exp)
		}
	}
	if !slices.Equal(handed, []string{"job-own", "job-guest"}) {
		t.Fatalf("handed %v, want the two jobs placed on this cluster and no other cluster's", handed)
	}
	// The platform experiment's id stays in the data address, which a workload saves under.
	for _, kept := range []string{"the hypothesis of", "the objective of", "the theory of", "hyp-job", "org-a", "org-b", `"platform_experiment_id":"pe-`, "RUNNING"} {
		if strings.Contains(rec.Body.String(), kept) {
			t.Errorf("the answer carries %q, which no runtime needs: %s", kept, rec.Body)
		}
	}
	if !slices.Equal(answer.CheckpointWindow, []string{"ran-here"}) {
		t.Fatalf("checkpoint_window = %v, want only the job the platform stopped on this cluster", answer.CheckpointWindow)
	}
	if strings.Contains(rec.Body.String(), "ran-on-another-orgs-cluster") {
		t.Fatalf("the answer names a job that ran on another cluster: %s", rec.Body)
	}
}

// The identity is bound before anything the runtime reports is recorded, and a refusal records
// and answers nothing.
func TestReconcileBindsTheReportedIdentityBeforeItRecordsAnything(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.desired["of-a"] = []*domain.Experiment{placedJob("job-own", "org-a")}

	if rec := w.reconcile(t, "of-a", idleReport("id-of-a")); rec.Code != http.StatusOK {
		t.Fatalf("first reconcile: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	if !slices.Equal(w.store.binds, []string{"of-a=id-of-a"}) || w.store.boundTo["of-a"] != "id-of-a" {
		t.Fatalf("binds = %v, bound to %q, want the cluster bound to the identity it reported", w.store.binds, w.store.boundTo["of-a"])
	}
	written := w.metricsWrites.Load()
	if written == 0 {
		t.Fatal("an accepted reconcile recorded no capacity")
	}

	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"an identity another cluster holds":       {db.ErrClusterIDBound, http.StatusConflict},
		"another identity than the cluster's own": {db.ErrClusterIDChanged, http.StatusConflict},
		"a cluster removed since":                 {db.ErrClusterNotFound, http.StatusNotFound},
	} {
		w.store.bindErr = tc.err
		rec := w.reconcile(t, "of-a", idleReport("id-stated"))
		if rec.Code != tc.want {
			t.Errorf("%s: got = %d %s, want = %d", name, rec.Code, rec.Body, tc.want)
		}
		if strings.Contains(rec.Body.String(), "job-own") || strings.Contains(rec.Body.String(), "token") {
			t.Errorf("%s: the refusal hands out the cluster's jobs: %s", name, rec.Body)
		}
		if got := w.metricsWrites.Load(); got != written {
			t.Errorf("%s: the refused report was recorded as capacity (%d writes, want %d)", name, got, written)
		}
	}

	w.store.bindErr = nil
	binds := len(w.store.binds)
	for _, invalid := range []string{"", "has/slash", "has space", strings.Repeat("a", domain.MaxClusterIDLength+1)} {
		rec := w.reconcile(t, "of-a", idleReport(invalid))
		if rec.Code != http.StatusUnprocessableEntity || len(w.store.binds) != binds || w.metricsWrites.Load() != written {
			t.Errorf("cluster_id %q: got = %d %s with %d binds, want 422 before anything is stored", invalid, rec.Code, rec.Body, len(w.store.binds)-binds)
		}
	}
}

// secret_env reaches the runtime on the spec of its own job, the resolved one included.
func TestReconcileHandsARuntimeTheSecretEnvOfItsJobs(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	withSecret := placedJob("job-secret", "org-a")
	withSecret.SecretEnv = map[string]string{"HF_TOKEN": "the-secret-value"}
	withSecret.ResolvedJob = &domain.JobSpec{CPU: "2"}
	w.store.desired["of-a"] = []*domain.Experiment{withSecret, placedJob("job-plain", "org-a")}

	rec := w.reconcile(t, "of-a", idleReport("id-of-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	var answer struct {
		Experiments []*domain.Experiment `json:"experiments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Experiments) != 2 {
		t.Fatalf("handed %d jobs, want 2", len(answer.Experiments))
	}
	if got := answer.Experiments[0].EffectiveJob().SecretEnv["HF_TOKEN"]; got != "the-secret-value" || answer.Experiments[0].Job.SecretEnv["HF_TOKEN"] != got {
		t.Fatalf("the job is handed without its secret_env: %s", rec.Body)
	}
	if answer.Experiments[1].Job.SecretEnv != nil || strings.Count(rec.Body.String(), "the-secret-value") != 2 {
		t.Fatalf("the secret is handed with a job it is not of: %s", rec.Body)
	}
}
