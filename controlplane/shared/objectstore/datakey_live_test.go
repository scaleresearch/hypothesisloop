package objectstore

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
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
	client, err := New("minio", endpoint, endpoint, "us-east-1", "hypothesisloop-data", "hypothesisloop", "hypothesisloop")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func storeStatus(t *testing.T, c *Client, method, key string, body []byte) int {
	t.Helper()
	req, err := c.SignedRequest(context.Background(), method, "/"+c.Bucket+"/"+key, nil, body)
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

// The store reads an object through the URL ListReadable hands out with no key at all, and
// refuses the same URL with its signature altered.
func TestLiveStoreServesAListedObjectByItsReadURL(t *testing.T) {
	root := liveStore(t)
	object := JobPrefix("pe-"+uuid.New().String()[:8], "agent", "job") + "ckpt.pt"
	t.Cleanup(func() { storeStatus(t, root, http.MethodDelete, object, nil) })
	if got := putStatus(t, root, object); got != http.StatusOK {
		t.Fatalf("seed %s: status %d", object, got)
	}
	objects, err := root.ListReadable(context.Background(), object)
	if err != nil || len(objects) != 1 {
		t.Fatalf("ListReadable = %+v, %v, want the one object", objects, err)
	}
	get := func(url string) (int, string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, body := get(objects[0].URL); status != http.StatusOK || body != "evidence" {
		t.Fatalf("GET the read URL: status %d, %q, want 200 and the object", status, body)
	}
	tampered := objects[0].URL[:len(objects[0].URL)-1] + map[bool]string{true: "1", false: "0"}[strings.HasSuffix(objects[0].URL, "0")]
	if status, _ := get(tampered); status != http.StatusForbidden {
		t.Fatalf("GET with an altered signature: status %d, want 403", status)
	}
}

func TestLiveStoreRefusesADeletedKey(t *testing.T) {
	root := liveStore(t)
	pe := "pe-" + uuid.New().String()[:8]
	key, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.CreateAgentDataKey(context.Background(), key, pe, "agent"); err != nil {
		t.Fatalf("CreateAgentDataKey: %v", err)
	}
	agent, err := New("minio", root.Endpoint, root.Endpoint, root.Region, root.Bucket, key.AccessKeyID, key.SecretAccessKey)
	if err != nil {
		t.Fatal(err)
	}
	object := AgentPrefix(pe, "agent") + "note"
	t.Cleanup(func() { storeStatus(t, root, http.MethodDelete, object, nil) })
	if got := putStatus(t, agent, object); got != http.StatusOK {
		t.Fatalf("write with a live key: status %d, want 200", got)
	}
	if err := root.DeleteDataKey(context.Background(), key.AccessKeyID); err != nil {
		t.Fatalf("DeleteDataKey: %v", err)
	}
	if got := putStatus(t, agent, object); got != http.StatusForbidden {
		t.Fatalf("write with a deleted key: status %d, want 403", got)
	}
	if err := root.DeleteDataKey(context.Background(), key.AccessKeyID); err != nil {
		t.Fatalf("DeleteDataKey of a key already deleted: %v", err)
	}
}
