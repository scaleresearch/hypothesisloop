package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bucketServer keeps objects as an S3 endpoint does, and refuses a request that is not signed.
func bucketServer(t *testing.T) (*Client, map[string][]byte) {
	t.Helper()
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key-id/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = body
		case http.MethodGet:
			body, found := objects[r.URL.Path]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<Error><Code>NoSuchKey</Code></Error>"))
				return
			}
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := New(srv.URL, "garage", "bucket", "key-id", "secret")
	if err != nil {
		t.Fatal(err)
	}
	return client, objects
}

func TestASessionIsKeptOutsideEveryPrefixAnAgentOrAJobKeyReads(t *testing.T) {
	client, objects := bucketServer(t)
	ctx := context.Background()
	snapshot := []byte{0x1f, 0x8b, 0, 0xff, 1, 2, 3}
	if err := client.PutSession(ctx, "pe-1", "ag-1", snapshot); err != nil {
		t.Fatal(err)
	}
	if err := client.PutSession(ctx, "pe-1", "", []byte("the coordinator's")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"/bucket/sessions/pe-1/agents/ag-1/session", "/bucket/sessions/pe-1/coordinator/session"} {
		if _, stored := objects[key]; !stored {
			t.Fatalf("stored under %v, want %s", objects, key)
		}
	}
	// What an agent's and a job's key open is the platform experiment's own prefix.
	for _, policy := range []func() (Policy, error){
		func() (Policy, error) { return AgentPolicy("bucket", "pe-1", "ag-1") },
		func() (Policy, error) { return JobPolicy("bucket", "pe-1", "ag-1", "job-1") },
	} {
		granted, err := policy()
		if err != nil {
			t.Fatal(err)
		}
		document, err := granted.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(document, "sessions") || strings.Contains(document, `bucket/*"`) {
			t.Fatalf("a key's policy reaches the sessions: %s", document)
		}
	}

	got, err := client.GetSession(ctx, "pe-1", "ag-1", 1<<20)
	if err != nil || !bytes.Equal(got, snapshot) {
		t.Fatalf("read back %v, %v, want the bytes stored", got, err)
	}
	if got, err := client.GetSession(ctx, "pe-1", "", 1<<20); err != nil || string(got) != "the coordinator's" {
		t.Fatalf("the coordinator's: %q, %v", got, err)
	}
	if _, err := client.GetSession(ctx, "pe-1", "ag-2", 1<<20); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an agent that stored none: %v, want ErrNoSession", err)
	}
	if _, err := client.GetSession(ctx, "pe-1", "ag-1", 3); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("a session above the bound: %v, want an error", err)
	}
}

func TestASessionIsStoredOnlyUnderIdentifiers(t *testing.T) {
	client, objects := bucketServer(t)
	ctx := context.Background()
	for _, ids := range [][2]string{{"pe-1/../pe-2", "ag-1"}, {"pe-1", "../coordinator"}, {"pe-*", "ag-1"}, {"", "ag-1"}, {"pe-1", "ag/1"}} {
		if err := client.PutSession(ctx, ids[0], ids[1], []byte("x")); err == nil {
			t.Errorf("stored a session for %q / %q", ids[0], ids[1])
		}
		if _, err := client.GetSession(ctx, ids[0], ids[1], 10); err == nil || errors.Is(err, ErrNoSession) {
			t.Errorf("read a session for %q / %q: %v", ids[0], ids[1], err)
		}
	}
	if err := client.PutSession(ctx, "pe-1", "ag-1", nil); err == nil {
		t.Error("stored an empty session")
	}
	if len(objects) != 0 {
		t.Fatalf("stored %v", objects)
	}
}

func TestAStoreThatRefusesASessionIsAnError(t *testing.T) {
	client, _ := bucketServer(t)
	client.AccessKeyID = "another-key"
	if err := client.PutSession(context.Background(), "pe-1", "ag-1", []byte("x")); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("put with a key the store refuses: %v", err)
	}
	if _, err := client.GetSession(context.Background(), "pe-1", "ag-1", 10); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("get with a key the store refuses: %v", err)
	}
}

func TestCodeIsKeptUnderItsAgentsOwnPrefixByCommit(t *testing.T) {
	client, objects := bucketServer(t)
	ctx := context.Background()
	sha := strings.Repeat("ab", 20)
	if err := client.PutCode(ctx, "pe-1", "ag-1", sha, []byte("a bundle")); err != nil {
		t.Fatal(err)
	}
	// Under the agent's prefix, which the per-agent ceiling measures and the experiment's
	// deletion takes with it.
	if _, stored := objects["/bucket/"+AgentPrefix("pe-1", "ag-1")+"code/"+sha+".bundle"]; !stored {
		t.Fatalf("stored under %v", objects)
	}
	if got, err := client.GetCode(ctx, "pe-1", "ag-1", sha, 1<<20); err != nil || string(got) != "a bundle" {
		t.Fatalf("read back %q, %v", got, err)
	}
	if _, err := client.GetCode(ctx, "pe-1", "ag-2", sha, 1<<20); !errors.Is(err, ErrNoCode) {
		t.Fatalf("another agent's place: %v, want ErrNoCode", err)
	}
	for _, bad := range []string{"", "main", strings.Repeat("AB", 20), sha + "/../x", sha[:39]} {
		if err := client.PutCode(ctx, "pe-1", "ag-1", bad, []byte("x")); err == nil {
			t.Errorf("a commit id of %q was accepted", bad)
		}
	}
	if err := client.PutCode(ctx, "pe-1", "../ag-2", sha, []byte("x")); err == nil {
		t.Error("an agent id that is a path was accepted")
	}
}
