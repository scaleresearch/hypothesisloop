package objectstore

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/google/uuid"
)

// The local stack's store (localdev/controlplane/docker-compose.yml).
const devStoreEndpoint = "http://localhost:9000"

func liveStore(t *testing.T) *Client {
	t.Helper()
	endpoint := os.Getenv("HYPOTHESISLOOP_TEST_STORE_URL")
	if endpoint == "" {
		endpoint = devStoreEndpoint
	}
	resp, err := http.Get(endpoint + "/minio/health/live")
	if err != nil {
		t.Skipf("no store at %s: %v", endpoint, err)
	}
	resp.Body.Close()
	client, err := New(endpoint, "us-east-1", "hypothesisloop-data", "hypothesisloop", "hypothesisloop")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func storeStatus(t *testing.T, c *Client, method, key string, body []byte) int {
	t.Helper()
	req, err := c.signedRequest(context.Background(), method, "/"+c.Bucket+"/"+key, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func putStatus(t *testing.T, c *Client, key string) int {
	return storeStatus(t, c, http.MethodPut, key, []byte("evidence"))
}

func getStatus(t *testing.T, c *Client, key string) int {
	return storeStatus(t, c, http.MethodGet, key, nil)
}

// The store itself enforces JobPolicy on a key made by CreateDataKey: the job writes only its own
// prefix and reads across its platform experiment.
func TestLiveStoreEnforcesTheJobKeyPolicy(t *testing.T) {
	root := liveStore(t)
	run := uuid.New().String()[:8]
	pe, otherPE := "pe-"+run, "otherpe-"+run
	key, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.CreateDataKey(context.Background(), key, pe, "agent", "job"); err != nil {
		t.Fatalf("CreateDataKey: %v", err)
	}
	own := JobPrefix(pe, "agent", "job") + "ckpt"
	rival := JobPrefix(pe, "rival", "job") + "ckpt"
	foreign := JobPrefix(otherPE, "agent", "job") + "ckpt"
	t.Cleanup(func() {
		for _, k := range []string{own, rival, foreign} {
			storeStatus(t, root, http.MethodDelete, k, nil)
		}
	})
	for _, k := range []string{rival, foreign} {
		if got := putStatus(t, root, k); got != http.StatusOK {
			t.Fatalf("seed %s: status %d", k, got)
		}
	}
	job := *root
	job.AccessKeyID, job.SecretAccessKey = key.AccessKeyID, key.SecretAccessKey

	if got := putStatus(t, &job, own); got != http.StatusOK {
		t.Fatalf("write own prefix: status %d, want 200", got)
	}
	if got := getStatus(t, &job, rival); got != http.StatusOK {
		t.Fatalf("read another job in the platform experiment: status %d, want 200", got)
	}
	if got := putStatus(t, &job, rival); got != http.StatusForbidden {
		t.Fatalf("overwrite another job's evidence: status %d, want 403", got)
	}
	if got := getStatus(t, &job, foreign); got != http.StatusForbidden {
		t.Fatalf("read another platform experiment: status %d, want 403", got)
	}
	if _, err := job.List(context.Background(), PlatformExperimentPrefix(otherPE)); err == nil {
		t.Fatal("listing another platform experiment succeeded, want 403")
	}
}
