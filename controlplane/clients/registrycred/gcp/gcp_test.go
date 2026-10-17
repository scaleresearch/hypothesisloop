package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/artifactregistry/v1"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// fakeGoogle is the Artifact Registry and IAM APIs of one project, as far as provisioning reads
// and writes them: repositories and their cleanup policies, service accounts and repository
// policies.
type fakeGoogle struct {
	mu           sync.Mutex
	repositories map[string]bool
	cleanup      map[string]artifactregistry.Repository
	accounts     map[string]bool
	policies     map[string]*artifactregistry.Policy
	writes       []string
	// creates counts every request to create a service account, the refused ones included.
	creates int
	// requests counts every request.
	requests int
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	refuse := func(code int, message string) {
		reply(code, map[string]any{"error": map[string]any{"code": code, "message": message}})
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(path, ":getIamPolicy"):
		policy := f.policies[strings.TrimSuffix(path, ":getIamPolicy")]
		if policy == nil {
			policy = &artifactregistry.Policy{}
		}
		reply(http.StatusOK, policy)
	case strings.HasSuffix(path, ":setIamPolicy"):
		var req artifactregistry.SetIamPolicyRequest
		_ = json.Unmarshal(body, &req)
		name := strings.TrimSuffix(path, ":setIamPolicy")
		f.policies[name] = req.Policy
		f.writes = append(f.writes, "policy "+name)
		reply(http.StatusOK, req.Policy)
	case strings.Contains(path, "/serviceAccounts/") && r.Method == http.MethodGet:
		id, _, _ := strings.Cut(path[strings.LastIndex(path, "/")+1:], "@")
		if !f.accounts[id] {
			refuse(http.StatusNotFound, "not found")
			return
		}
		reply(http.StatusOK, iam.ServiceAccount{Name: path})
	case strings.HasSuffix(path, "/serviceAccounts") && r.Method == http.MethodPost:
		f.creates++
		var req iam.CreateServiceAccountRequest
		_ = json.Unmarshal(body, &req)
		if f.accounts[req.AccountId] {
			refuse(http.StatusConflict, "already exists")
			return
		}
		f.accounts[req.AccountId] = true
		f.writes = append(f.writes, "account "+req.AccountId)
		reply(http.StatusOK, iam.ServiceAccount{Name: req.AccountId})
	case strings.HasSuffix(path, "/repositories") && r.Method == http.MethodPost:
		name := path + "/" + r.URL.Query().Get("repositoryId")
		if f.repositories[name] {
			refuse(http.StatusConflict, "already exists")
			return
		}
		var req artifactregistry.Repository
		_ = json.Unmarshal(body, &req)
		f.repositories[name] = true
		f.cleanup[name] = artifactregistry.Repository{CleanupPolicies: req.CleanupPolicies, CleanupPolicyDryRun: req.CleanupPolicyDryRun}
		f.writes = append(f.writes, "repository "+name)
		reply(http.StatusOK, artifactregistry.Operation{Name: "operations/1", Done: true})
	case strings.Contains(path, "/repositories/") && r.Method == http.MethodGet:
		if !f.repositories[path] {
			refuse(http.StatusNotFound, "not found")
			return
		}
		repository := f.cleanup[path]
		repository.Name = path
		reply(http.StatusOK, repository)
	case strings.Contains(path, "/repositories/") && r.Method == http.MethodPatch:
		if r.URL.Query().Get("updateMask") != "cleanup_policies,cleanup_policy_dry_run" {
			refuse(http.StatusBadRequest, "update mask "+r.URL.Query().Get("updateMask"))
			return
		}
		var req artifactregistry.Repository
		_ = json.Unmarshal(body, &req)
		f.cleanup[path] = artifactregistry.Repository{CleanupPolicies: req.CleanupPolicies, CleanupPolicyDryRun: req.CleanupPolicyDryRun}
		f.writes = append(f.writes, "cleanup "+path)
		reply(http.StatusOK, req)
	default:
		refuse(http.StatusNotImplemented, r.Method+" "+path)
	}
}

func (f *fakeGoogle) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// minted mints a token of each account it is asked for, numbered, living what it is asked for but
// at most lifetime from clock.
type minted struct {
	clock    *time.Time
	lifetime time.Duration
	accounts []string
}

func (m *minted) mint(_ context.Context, account string, lifetime time.Duration) (*oauth2.Token, error) {
	m.accounts = append(m.accounts, account)
	return &oauth2.Token{AccessToken: fmt.Sprintf("%s-%d", account, len(m.accounts)), Expiry: m.clock.Add(min(lifetime, m.lifetime))}, nil
}

func testRegistry(t *testing.T, google *fakeGoogle, m *minted) *Registry {
	t.Helper()
	server := httptest.NewServer(google)
	t.Cleanup(server.Close)
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(server.URL + "/"), option.WithHTTPClient(server.Client())}
	ar, err := artifactregistry.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := iam.NewService(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return &Registry{project: "proj", location: "europe-west4", keepFor: 30*24*time.Hour + 90*time.Minute, ar: ar, iam: accounts, mint: m.mint, now: func() time.Time { return *m.clock }, tokens: map[string]*oauth2.Token{}}
}

func newGoogle() *fakeGoogle {
	return &fakeGoogle{repositories: map[string]bool{}, cleanup: map[string]artifactregistry.Repository{}, accounts: map[string]bool{}, policies: map[string]*artifactregistry.Policy{}}
}

// Provisioning an org creates its repository, its pusher and puller, and binds them as writer and
// reader of it, beside whatever the policy already grants. Doing it again finds it all there and
// changes nothing, and a push login only mints the pusher's token.
func TestProvisioningAnOrgCreatesItsRepositoryAndAccountsOnce(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	google, m := newGoogle(), &minted{clock: &now, lifetime: time.Hour}
	r := testRegistry(t, google, m)
	repository := r.repositoryName("org-a")
	google.policies[repository] = &artifactregistry.Policy{Bindings: []*artifactregistry.Binding{{Role: "roles/artifactregistry.admin", Members: []string{"user:ops@example.com"}}}}

	if err := r.Provision(context.Background(), "org-a"); err != nil {
		t.Fatal(err)
	}
	push, err := r.PushLogin(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	pusher, puller := r.pusher("org-a"), r.puller("org-a")
	if push.Registry != "europe-west4-docker.pkg.dev" || push.Username != "oauth2accesstoken" || push.Password != pusher+"-1" || !push.ExpiresAt.Equal(now.Add(pushLifetime)) {
		t.Fatalf("push login = %+v, want the pusher's token for the registry host", push)
	}
	pusherID, _, _ := strings.Cut(pusher, "@")
	pullerID, _, _ := strings.Cut(puller, "@")
	if want := []string{"repository " + repository, "account " + pusherID, "account " + pullerID, "policy " + repository}; !slices.Equal(google.written(), want) {
		t.Fatalf("wrote %v, want %v", google.written(), want)
	}
	members := map[string][]string{}
	for _, b := range google.policies[repository].Bindings {
		members[b.Role] = b.Members
	}
	if !slices.Equal(members[writerRole], []string{"serviceAccount:" + pusher}) || !slices.Equal(members[readerRole], []string{"serviceAccount:" + puller}) ||
		!slices.Equal(members["roles/artifactregistry.admin"], []string{"user:ops@example.com"}) {
		t.Fatalf("policy = %v, want the pusher a writer, the puller a reader and the admin kept", members)
	}

	before, creates := len(google.written()), google.creates
	if err := r.Provision(context.Background(), "org-a"); err != nil {
		t.Fatal(err)
	}
	if got := google.written()[before:]; len(got) != 0 {
		t.Fatalf("a second provisioning wrote %v", got)
	}
	// Google counts a refused create against its per-minute quota of creates, so accounts that
	// are there are read and never asked to be created again.
	if google.creates != creates {
		t.Fatalf("a second provisioning asked to create %d service accounts", google.creates-creates)
	}
	// A push login is minted for its call alone, so none outlives the run it was handed to by
	// more than pushLifetime, and asks Google for nothing but the token.
	requests := google.requests
	again, err := r.PushLogin(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if google.requests != requests {
		t.Fatalf("a push login made %d requests to Google", google.requests-requests)
	}
	if again.Password == push.Password || again.ExpiresAt.Sub(now) > pushLifetime {
		t.Fatalf("second push login = %+v, want a token of its own living at most %s", again, pushLifetime)
	}

	// Another org gets a repository and accounts of its own.
	if err := r.Provision(context.Background(), "org-b"); err != nil {
		t.Fatal(err)
	}
	if r.repositoryName("org-b") == repository || r.pusher("org-b") == pusher || !google.repositories[r.repositoryName("org-b")] {
		t.Fatalf("org-b shares org-a's repository or accounts")
	}
}

// A repository is created keeping the newest version of each experiment's image and deleting every
// other once no job can pull it. One provisioned without them, or with them only reported, gets
// them on the next provisioning, and one that has them is left as it is.
func TestProvisioningGivesAnOrgsRepositoryItsCleanupPolicies(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	google, m := newGoogle(), &minted{clock: &now, lifetime: time.Hour}
	r := testRegistry(t, google, m)
	ctx := context.Background()
	want := map[string]artifactregistry.CleanupPolicy{
		"keep-newest": {Id: "keep-newest", Action: "KEEP", MostRecentVersions: &artifactregistry.CleanupPolicyMostRecentVersions{KeepCount: 1}},
		"delete-unpullable": {Id: "delete-unpullable", Action: "DELETE",
			Condition: &artifactregistry.CleanupPolicyCondition{TagState: "ANY", OlderThan: "2597400s"}},
	}
	enforced := func(name string) bool {
		got := google.cleanup[name]
		return !got.CleanupPolicyDryRun && reflect.DeepEqual(got.CleanupPolicies, want)
	}

	if err := r.Provision(ctx, "org-a"); err != nil {
		t.Fatal(err)
	}
	created := r.repositoryName("org-a")
	if !enforced(created) {
		t.Fatalf("created with %+v, want %+v enforced", google.cleanup[created], want)
	}

	existing := r.repositoryName("org-b")
	for _, before := range []artifactregistry.Repository{{}, {CleanupPolicies: want, CleanupPolicyDryRun: true}} {
		google.repositories[existing], google.cleanup[existing] = true, before
		if err := r.Provision(ctx, "org-b"); err != nil {
			t.Fatal(err)
		}
		if !enforced(existing) {
			t.Fatalf("an existing repository with %+v has %+v after provisioning, want %+v enforced", before, google.cleanup[existing], want)
		}
	}

	before := len(google.written())
	for _, orgID := range []string{"org-a", "org-b"} {
		if err := r.Provision(ctx, orgID); err != nil {
			t.Fatal(err)
		}
	}
	if got := google.written()[before:]; len(got) != 0 {
		t.Fatalf("provisioning repositories that have their policies wrote %v", got)
	}
}

// A job pulls an image of its org's repository with the puller's token, minted without a request
// to Google, and every other image, a public one or one of another org, with none.
func TestAPullLoginIsThePullersTokenForAnImageOfTheOrgsRepositoryOnly(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	google, m := newGoogle(), &minted{clock: &now, lifetime: time.Hour}
	r := testRegistry(t, google, m)
	ctx := context.Background()

	for _, image := range []string{"docker.io/library/python@sha256:1", r.Repository("org-b", "pe-2") + "@sha256:2", "ghcr.io/acme/model@sha256:3"} {
		if login, err := r.PullLogin(ctx, "org-a", image); login != nil || err != nil {
			t.Errorf("%s: login %+v, %v, want none", image, login, err)
		}
	}
	if len(m.accounts) != 0 {
		t.Fatalf("minted for %v", m.accounts)
	}
	pull, err := r.PullLogin(ctx, "org-a", r.Repository("org-a", "pe-1")+"@sha256:4")
	if err != nil || pull == nil || pull.Password != r.puller("org-a")+"-1" {
		t.Fatalf("pull login = %+v, %v, want the puller's token", pull, err)
	}
	if google.requests != 0 {
		t.Fatalf("a pull login made %d requests to Google", google.requests)
	}
}

// A token is handed out again until it has less than MinLifetime left, and only then minted anew,
// each service account's on its own.
func TestATokenIsReusedUntilItHasLessThanMinLifetimeLeft(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	google, m := newGoogle(), &minted{clock: &now, lifetime: time.Hour}
	r := testRegistry(t, google, m)
	ctx := context.Background()
	image := func(orgID string) string { return r.Repository(orgID, "pe-1") + ":v1" }
	pull := func() string {
		t.Helper()
		login, err := r.PullLogin(ctx, "org-a", image("org-a"))
		if err != nil {
			t.Fatal(err)
		}
		if left := login.ExpiresAt.Sub(now); left < MinLifetime {
			t.Fatalf("handed out a login with %s left", left)
		}
		return login.Password
	}

	first := pull()
	now = now.Add(time.Hour - MinLifetime)
	if again := pull(); again != first {
		t.Fatalf("with exactly MinLifetime left: %s, want %s again", again, first)
	}
	if other, err := r.PullLogin(ctx, "org-b", image("org-b")); err != nil || other.Password == first {
		t.Fatalf("org-b's pull login = %+v, %v, want a token of its own puller", other, err)
	}
	now = now.Add(time.Second)
	if fresh := pull(); fresh == first {
		t.Fatalf("a token with less than MinLifetime left was handed out again")
	}
	if len(m.accounts) != 3 {
		t.Fatalf("minted for %v, want org-a's puller twice and org-b's once", m.accounts)
	}

	// A fresh token that lives less than MinLifetime is refused, never handed out.
	m.lifetime = MinLifetime - time.Minute
	now = now.Add(time.Hour)
	if login, err := r.PullLogin(ctx, "org-a", image("org-a")); err == nil {
		t.Fatalf("handed out %+v, which lives %s", login, m.lifetime)
	}
}

// An image is the org's when it is under the org's repository, and managed when it is under any
// org's: an image elsewhere in the project or on another host is neither.
func TestAnImageIsAnOrgsOnlyUnderItsRepository(t *testing.T) {
	r := &Registry{project: "proj", location: "europe-west4"}
	own := r.Repository("org-a", "pe-1") + "@sha256:1"
	other := r.Repository("org-b", "pe-2") + ":v1"
	if !strings.HasPrefix(own, "europe-west4-docker.pkg.dev/proj/hl-") {
		t.Fatalf("repository %s", own)
	}
	if !r.Holds("org-a", own) || r.Holds("org-a", other) || r.Holds("org-b", own) {
		t.Errorf("holds: own %v, other's %v", r.Holds("org-a", own), r.Holds("org-a", other))
	}
	for image, managed := range map[string]bool{
		own: true, other: true,
		"europe-west4-docker.pkg.dev/proj/team/model:v1":    false,
		"europe-west4-docker.pkg.dev/otherproj/hl-abc/x:v1": false,
		"us-docker.pkg.dev/proj/" + r.repositoryID("org-a"): false,
		"docker.io/library/python:3.12":                     false,
	} {
		if r.Managed(image) != managed {
			t.Errorf("managed(%s) = %v, want %v", image, !managed, managed)
		}
	}
}
