package db

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore/minio"
)

var testGrace = DataKeyGrace{Job: 10 * time.Minute, Agent: domain.AgentWindDown}

func liveDataStore(t *testing.T) *objectstore.Client {
	t.Helper()
	endpoint := os.Getenv("HYPOTHESISLOOP_TEST_STORE_URL")
	if endpoint == "" {
		t.Skip("HYPOTHESISLOOP_TEST_STORE_URL is not set")
	}
	resp, err := http.Get(endpoint + "/minio/health/live")
	if err != nil {
		t.Skipf("no store at %s: %v", endpoint, err)
	}
	resp.Body.Close()
	client, err := objectstore.New(endpoint, endpoint, "us-east-1", "hypothesisloop-data", "hypothesisloop", "hypothesisloop",
		func(c *objectstore.Client) objectstore.KeyIssuer { return minio.New(c, c.SecretAccessKey) })
	if err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureBucket(context.Background()); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return client
}

// putAs is the status the store answers a write of object with key.
func putAs(t *testing.T, root *objectstore.Client, key domain.DataKey, object string) int {
	t.Helper()
	holder, err := objectstore.New(root.Endpoint, root.Endpoint, root.Region, root.Bucket, key.AccessKeyID, key.SecretAccessKey,
		func(c *objectstore.Client) objectstore.KeyIssuer { return minio.New(c, c.SecretAccessKey) })
	if err != nil {
		t.Fatal(err)
	}
	req, err := holder.SignedRequest(context.Background(), http.MethodPut, "/"+root.Bucket+"/"+object, nil, []byte("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := holder.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func endedKeysOf(t *testing.T, store *Store) []EndedDataKey {
	t.Helper()
	keys, err := store.EndedDataKeys(context.Background(), testGrace)
	if err != nil {
		t.Fatalf("EndedDataKeys: %v", err)
	}
	return keys
}

// A job's key outlives its end by the grace its workload has to save and shut down, then
// leaves the store and the row; a deletion that fails keeps it for the next pass.
func TestDropDataKeyTakesAnEndedJobsKeyOutOfTheStore(t *testing.T) {
	root := liveDataStore(t)
	store, exp := admitDataKeyFixture(t)
	ctx := context.Background()
	key, err := objectstore.NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	exp.DataKey = key
	if _, err := store.AdmitExperimentTx(ctx, exp, noUsage, SubmissionRateLimit{}, func(ctx context.Context) error {
		return root.CreateDataKey(ctx, key, exp.PlatformExperimentID, exp.AgentID, exp.ID)
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	object := objectstore.JobPrefix(exp.PlatformExperimentID, exp.AgentID, exp.ID) + "checkpoint"
	if got := putAs(t, root, key, object); got != http.StatusOK {
		t.Fatalf("write with the job's key: status %d, want 200", got)
	}
	want := EndedDataKey{ExperimentID: exp.ID, AccessKeyID: key.AccessKeyID}

	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a queued job's key is listed as ended")
	}
	if _, err := store.pool.pool.Exec(ctx, `UPDATE experiments SET status = 'EVICTED', eviction_reason = 'preempted', not_admitted_reason = NULL, ended_at = now() WHERE id = $1`, exp.ID); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a job that ended within its grace has its key listed")
	}
	if _, err := store.pool.pool.Exec(ctx, `UPDATE experiments SET ended_at = now() - interval '11 minutes' WHERE id = $1`, exp.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("the key of a job past its grace is not listed")
	}

	if err := store.DropDataKey(ctx, want, testGrace, func(context.Context, string) error { return errors.New("store down") }); err == nil {
		t.Fatal("DropDataKey with a failing deletion: got nil, want the error")
	}
	if !slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a key whose deletion failed is no longer listed")
	}

	if err := store.DropDataKey(ctx, want, testGrace, root.DeleteDataKey); err != nil {
		t.Fatalf("DropDataKey: %v", err)
	}
	if got := putAs(t, root, key, object); got != http.StatusForbidden {
		t.Fatalf("write with the dropped key: status %d, want 403", got)
	}
	got, err := store.GetExperiment(ctx, exp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DataKey != (domain.DataKey{}) {
		t.Fatalf("row keeps key %+v after the drop", got.DataKey)
	}
	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a dropped key is still listed")
	}
}

// An agent's key outlives the end of its run by its wind-down and then goes; one whose run
// goes on, or whose key changed since it was listed, is left alone.
func TestDropDataKeyTakesAnEndedAgentsKeyOutOfTheStore(t *testing.T) {
	root := liveDataStore(t)
	pool := eventsTestDB(t)
	store := NewStore(pool, 3)
	ctx := context.Background()
	suffix := uuid.New().String()[:8]
	agent, pe := "dk-agent-"+suffix, "dk-pe-"+suffix
	createTestAgent(t, pool, agent)
	createTestPE(t, pool, pe, 10)
	if ok, err := store.Signup(ctx, pe, domain.AgentSignup{Actor: testActor, AgentID: agent, Role: domain.SignupRoleCompetitor}); err != nil || !ok {
		t.Fatalf("signup: %v %v", ok, err)
	}
	key, err := store.AgentDataKey(ctx, pe, agent, func(ctx context.Context) (domain.DataKey, error) {
		key, err := objectstore.NewDataKey()
		if err != nil {
			return domain.DataKey{}, err
		}
		return key, root.CreateAgentDataKey(ctx, key, pe, agent)
	})
	if err != nil {
		t.Fatalf("AgentDataKey: %v", err)
	}
	object := objectstore.AgentPrefix(pe, agent) + "note"
	if got := putAs(t, root, key, object); got != http.StatusOK {
		t.Fatalf("write with the agent's key: status %d, want 200", got)
	}
	want := EndedDataKey{PlatformExperimentID: pe, AgentID: agent, AccessKeyID: key.AccessKeyID}

	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("the key of an agent at work is listed")
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE experiment_signups SET finished_at = now() WHERE platform_experiment_id = $1 AND agent_id = $2`, pe, agent); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("the key of an agent in its wind-down is listed")
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE experiment_signups SET finished_at = now() - interval '3 minutes' WHERE platform_experiment_id = $1 AND agent_id = $2`, pe, agent); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("the key of an agent past its wind-down is not listed")
	}

	stale := want
	stale.AccessKeyID = "other"
	deleted := 0
	if err := store.DropDataKey(ctx, stale, testGrace, func(context.Context, string) error { deleted++; return nil }); err != nil || deleted != 0 {
		t.Fatalf("DropDataKey of a key the row no longer holds: err %v, %d deletions, want none", err, deleted)
	}

	if err := store.DropDataKey(ctx, want, testGrace, root.DeleteDataKey); err != nil {
		t.Fatalf("DropDataKey: %v", err)
	}
	if got := putAs(t, root, key, object); got != http.StatusForbidden {
		t.Fatalf("write with the dropped key: status %d, want 403", got)
	}
	if slices.Contains(endedKeysOf(t, store), want) {
		t.Fatal("a dropped key is still listed")
	}
}
