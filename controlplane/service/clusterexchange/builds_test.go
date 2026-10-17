package clusterexchange

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const pushedDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func assignedBuild(key string, attempt int) *domain.Environment {
	return &domain.Environment{Key: key, OrgID: "org-a", Base: "registry.test/job:7", Dockerfile: "ARG BASE\nFROM ${BASE}\nRUN true\n",
		State: domain.EnvironmentBuilding, ClusterName: "of-a", Attempt: attempt}
}

func (w *reconcileWorld) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := authtest.Authorize(httptest.NewRequest("POST", path, bytes.NewReader(data)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	return rec
}

type buildsAnswer struct {
	Builds       []domain.EnvironmentBuild `json:"builds"`
	RegistryPull *domain.RegistryLogin     `json:"registry_pull"`
}

func (w *reconcileWorld) builds(t *testing.T, report map[string]any) buildsAnswer {
	t.Helper()
	rec := w.reconcile(t, "of-a", report)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	var answer buildsAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	return answer
}

func reportingBuilds(builds bool) map[string]any {
	report := idleReport("id-of-a")
	report["builds"] = builds
	return report
}

// A cluster builds what it says it can, and is answered the builds assigned to it, each with
// what it is built from and where it is pushed.
func TestReconcileRecordsWhetherTheRuntimeBuildsAndAnswersItsAssignedBuilds(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 1), assignedBuild("key-2", 3)}
	answer := w.builds(t, reportingBuilds(true))
	if !w.store.builds["of-a"] {
		t.Error("a runtime that reports builds is not recorded as building")
	}
	want := []domain.EnvironmentBuild{
		{Key: "key-1", Attempt: 1, Base: "registry.test/job:7", Dockerfile: "ARG BASE\nFROM ${BASE}\nRUN true\n", Repository: testRepository, CacheScope: domain.EnvironmentCacheScope("org-a", "")},
		{Key: "key-2", Attempt: 3, Base: "registry.test/job:7", Dockerfile: "ARG BASE\nFROM ${BASE}\nRUN true\n", Repository: testRepository, CacheScope: domain.EnvironmentCacheScope("org-a", "")},
	}
	if !slices.Equal(answer.Builds, want) {
		t.Errorf("builds = %+v, want %+v", answer.Builds, want)
	}
	if answer.RegistryPull != nil || w.logins.pulls != 0 {
		t.Errorf("registry_pull = %+v, want none while no listed job runs in an environment", answer.RegistryPull)
	}

	w.builds(t, idleReport("id-of-a"))
	if w.store.builds["of-a"] {
		t.Error("a runtime that no longer reports builds is still recorded as building")
	}
}

func TestReconcileHandsAPullLoginOnlyWhenAListedJobRunsInAnEnvironment(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.desired["of-a"] = []*domain.Experiment{placedJob("job-plain", "org-a")}
	if answer := w.builds(t, idleReport("id-of-a")); answer.RegistryPull != nil {
		t.Fatalf("registry_pull = %+v, want none for a job in its job image", answer.RegistryPull)
	}
	inEnvironment := placedJob("job-env", "org-a")
	inEnvironment.EnvironmentKey = "key-1"
	w.store.desired["of-a"] = append(w.store.desired["of-a"], inEnvironment)
	answer := w.builds(t, idleReport("id-of-a"))
	if answer.RegistryPull == nil || answer.RegistryPull.Password != "pull-1" || answer.RegistryPull.Registry != "registry.test" {
		t.Fatalf("registry_pull = %+v, want the puller's login", answer.RegistryPull)
	}
	if answer.Builds == nil {
		t.Error("builds is absent, want an empty list")
	}
}

// A pull login that cannot be minted fails only the pulls: the runtime is still answered what
// should run, and keeps every workload that does.
func TestReconcileAnswersWithoutAPullLoginWhenNoneCanBeMinted(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.logins.pullErr = errors.New("token endpoint down")
	inEnvironment := placedJob("job-env", "org-a")
	inEnvironment.EnvironmentKey = "key-1"
	w.store.desired["of-a"] = []*domain.Experiment{placedJob("job-plain", "org-a"), inEnvironment}
	rec := w.reconcile(t, "of-a", idleReport("id-of-a"))
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile: got = %d %s, want = 200", rec.Code, rec.Body)
	}
	var answer struct {
		buildsAnswer
		Experiments []*domain.Experiment `json:"experiments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.RegistryPull != nil || len(answer.Experiments) != 2 {
		t.Errorf("registry_pull = %+v with %d experiments, want none with both", answer.RegistryPull, len(answer.Experiments))
	}
}

// A deployment with no environments section counts no cluster as building and hands out nothing.
func TestADeploymentWithoutEnvironmentsCountsNoClusterAsBuilding(t *testing.T) {
	w := newWorld(t, "of-a", func(base *reconcileStore) Store { return base }, false)
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 1)}
	inEnvironment := placedJob("job-env", "org-a")
	inEnvironment.EnvironmentKey = "key-1"
	w.store.desired["of-a"] = []*domain.Experiment{inEnvironment}
	answer := w.builds(t, reportingBuilds(true))
	if w.store.builds["of-a"] || len(answer.Builds) != 0 || answer.RegistryPull != nil {
		t.Errorf("recorded builds %v and answered %+v, want the cluster not building and nothing handed out", w.store.builds["of-a"], answer)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		w.post(t, "/of-a/builds/key-1/credential", domain.EnvironmentBuildCredentialRequest{Attempt: 1}),
		w.post(t, "/of-a/builds/key-1", domain.EnvironmentBuildReport{Attempt: 1, Outcome: domain.EnvironmentBuildPushed, Digest: pushedDigest}),
		w.post(t, "/of-a/builds/key-1/loaded", domain.EnvironmentBuildLoaded{Attempt: 1, Node: "node-1", Digest: pushedDigest}),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("got = %d %s, want = 409", rec.Code, rec.Body)
		}
	}
}

// A push login goes only to the cluster the build is assigned to, at the attempt it is at.
func TestAPushLoginIsHandedOutOnlyForTheAssignedAttempt(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 2)}
	rec := w.post(t, "/of-a/builds/key-1/credential", domain.EnvironmentBuildCredentialRequest{Attempt: 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("got = %d %s, want = 200", rec.Code, rec.Body)
	}
	var login domain.RegistryLogin
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login.Password != "push-1" || login.Username != "oauth2accesstoken" || login.Registry != "registry.test" {
		t.Errorf("login = %+v, want the pusher's", login)
	}
	if rec := w.post(t, "/of-a/builds/key-1/credential", domain.EnvironmentBuildCredentialRequest{Attempt: 1}); rec.Code != http.StatusConflict {
		t.Errorf("a superseded attempt: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	if rec := w.post(t, "/of-a/builds/key-9/credential", domain.EnvironmentBuildCredentialRequest{Attempt: 2}); rec.Code != http.StatusConflict {
		t.Errorf("a key not assigned: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	if rec := w.post(t, "/of-b/builds/key-1/credential", domain.EnvironmentBuildCredentialRequest{Attempt: 2}); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("another cluster's path: got = %d, want it refused before the handler", rec.Code)
	}
	if w.logins.pushes != 1 {
		t.Errorf("minted %d push logins, want only the one handed out", w.logins.pushes)
	}
}

// A build reads its repository through the git proxy with a token of that attempt of that build,
// and never holds a credential of the git host.
func TestAGitCredentialIsABuildTokenForTheProxyAndNoHostCredential(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	env := assignedBuild("key-1", 2)
	env.ProjectID, env.GitRepository = "prj-1", "acme/model.git"
	noRepository := assignedBuild("key-2", 1)
	w.store.assigned["of-a"] = []*domain.Environment{env, noRepository}
	rec := w.post(t, "/of-a/builds/key-1/git-credential", domain.EnvironmentBuildCredentialRequest{Attempt: 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("got = %d %s, want = 200", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "host-password") || strings.Contains(rec.Body.String(), "platform:") {
		t.Errorf("the answer %s carries the git host's credential", rec.Body)
	}
	var credential domain.EnvironmentGitCredential
	if err := json.Unmarshal(rec.Body.Bytes(), &credential); err != nil {
		t.Fatal(err)
	}
	want, _ := auth.Mint("test-signing-secret-of-enough-length", auth.BuildScope("of-a", "key-1", 2))
	if credential != (domain.EnvironmentGitCredential{ProxyURL: "https://api.test/git/builds/key-1", RepositoryURL: "https://git.test/acme/model.git", Token: want}) {
		t.Errorf("credential = %+v, want the proxy, the repository's address and the build's token", credential)
	}
	if _, err := credential.GitConfig(); err != nil {
		t.Errorf("the credential makes no git config: %v", err)
	}
	if rec := w.post(t, "/of-a/builds/key-1/git-credential", domain.EnvironmentBuildCredentialRequest{Attempt: 1}); rec.Code != http.StatusConflict {
		t.Errorf("a superseded attempt: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	if rec := w.post(t, "/of-a/builds/key-2/git-credential", domain.EnvironmentBuildCredentialRequest{Attempt: 1}); rec.Code != http.StatusNotFound {
		t.Errorf("an environment of no repository: got = %d %s, want = 404", rec.Code, rec.Body)
	}
}

func TestABuildReportIsRecordedFencedOnItsAttempt(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 1), assignedBuild("key-2", 1), assignedBuild("key-3", 1)}
	for _, c := range []struct {
		key    string
		report domain.EnvironmentBuildReport
	}{
		{"key-1", domain.EnvironmentBuildReport{Attempt: 1, Outcome: domain.EnvironmentBuildPushed, Digest: pushedDigest}},
		{"key-2", domain.EnvironmentBuildReport{Attempt: 1, Outcome: domain.EnvironmentBuildFileFault, LogTail: "RUN false: exit code 1"}},
		{"key-3", domain.EnvironmentBuildReport{Attempt: 1, Outcome: domain.EnvironmentBuildPlatformFault, LogTail: "pull base: timeout"}},
	} {
		if rec := w.post(t, "/of-a/builds/"+c.key, c.report); rec.Code != http.StatusNoContent {
			t.Fatalf("%s: got = %d %s, want = 204", c.key, rec.Code, rec.Body)
		}
	}
	want := []string{
		`key-1 fault="" image="` + testRepository + "@" + pushedDigest + `" log=""`,
		`key-2 fault="file" image="" log="RUN false: exit code 1"`,
		`key-3 fault="platform" image="" log="pull base: timeout"`,
	}
	if !slices.Equal(w.store.finished, want) {
		t.Errorf("finished = %q, want %q", w.store.finished, want)
	}
	if rec := w.post(t, "/of-a/builds/key-1", domain.EnvironmentBuildReport{Attempt: 2, Outcome: domain.EnvironmentBuildPushed, Digest: pushedDigest}); rec.Code != http.StatusConflict {
		t.Errorf("a report of an attempt not assigned: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	if got := w.wakes.Load(); got != 3 {
		t.Errorf("admission woken %d times, want once per recorded report and not for the refused one", got)
	}
}

// A build held on its node is recorded as the repository's image at its digest on that node, and
// wakes admission, so a job that fits there starts while the push goes on.
func TestABuildLoadedOnANodeIsRecordedFencedOnItsAttemptAndWakesAdmission(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 1)}
	if rec := w.post(t, "/of-a/builds/key-1/loaded", domain.EnvironmentBuildLoaded{Attempt: 1, Node: "node-1", Digest: pushedDigest}); rec.Code != http.StatusNoContent {
		t.Fatalf("got = %d %s, want = 204", rec.Code, rec.Body)
	}
	if want := []string{"key-1 node-1 " + testRepository + "@" + pushedDigest}; !slices.Equal(w.store.loaded, want) {
		t.Errorf("loaded = %q, want %q", w.store.loaded, want)
	}
	if w.wakes.Load() != 1 {
		t.Errorf("admission woken %d times, want once", w.wakes.Load())
	}
	if rec := w.post(t, "/of-a/builds/key-1/loaded", domain.EnvironmentBuildLoaded{Attempt: 2, Node: "node-1", Digest: pushedDigest}); rec.Code != http.StatusConflict {
		t.Errorf("an attempt not assigned: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	for name, loaded := range map[string]domain.EnvironmentBuildLoaded{
		"no digest":         {Attempt: 1, Node: "node-1"},
		"a tag":             {Attempt: 1, Node: "node-1", Digest: "latest"},
		"no node":           {Attempt: 1, Digest: pushedDigest},
		"a node with space": {Attempt: 1, Node: "node 1", Digest: pushedDigest},
		"a node too long":   {Attempt: 1, Node: strings.Repeat("n", domain.MaxNodeNameBytes+1), Digest: pushedDigest},
	} {
		if rec := w.post(t, "/of-a/builds/key-1/loaded", loaded); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got = %d %s, want = 400", name, rec.Code, rec.Body)
		}
	}
	if len(w.store.loaded) != 1 || w.wakes.Load() != 1 {
		t.Errorf("loaded = %q after %d wakes, want only the first recorded", w.store.loaded, w.wakes.Load())
	}
}

func TestABuildReportThatSaysTooLittleOrTooMuchIsRefused(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 1)}
	for name, report := range map[string]domain.EnvironmentBuildReport{
		"pushed with no digest":        {Attempt: 1, Outcome: domain.EnvironmentBuildPushed},
		"pushed with a tag":            {Attempt: 1, Outcome: domain.EnvironmentBuildPushed, Digest: "latest"},
		"pushed with another digest":   {Attempt: 1, Outcome: domain.EnvironmentBuildPushed, Digest: "sha512:" + strings.Repeat("ab", 64)},
		"a fault with a digest":        {Attempt: 1, Outcome: domain.EnvironmentBuildFileFault, Digest: pushedDigest},
		"an outcome nobody wrote":      {Attempt: 1, Outcome: "maybe"},
		"a log tail past the boundary": {Attempt: 1, Outcome: domain.EnvironmentBuildFileFault, LogTail: strings.Repeat("x", domain.MaxEnvironmentLogBytes+1)},
	} {
		if rec := w.post(t, "/of-a/builds/key-1", report); rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got = %d %s, want it refused", name, rec.Code, rec.Body)
		}
	}
	if len(w.store.finished) != 0 {
		t.Errorf("finished = %q, want nothing recorded", w.store.finished)
	}
}

// The cluster that builds an environment tells the step it is at, which the job waiting for it
// shows: a job once waited 19 minutes on a build that said nothing.
func TestABuildsProgressIsRecordedFencedOnItsAttemptAndOneLine(t *testing.T) {
	w := newReconcileWorld(t, "of-a")
	w.store.assigned["of-a"] = []*domain.Environment{assignedBuild("key-1", 1)}
	if rec := w.post(t, "/of-a/builds/key-1/progress", domain.EnvironmentBuildProgress{Attempt: 1, Step: "[2/3] RUN apt-get install gcc: started"}); rec.Code != http.StatusNoContent {
		t.Fatalf("got = %d %s, want = 204", rec.Code, rec.Body)
	}
	if rec := w.post(t, "/of-a/builds/key-1/progress", domain.EnvironmentBuildProgress{Attempt: 2, Step: "late"}); rec.Code != http.StatusConflict {
		t.Errorf("an attempt not assigned: got = %d %s, want = 409", rec.Code, rec.Body)
	}
	for name, step := range map[string]string{"empty": "", "two lines": "a\nb", "past the boundary": strings.Repeat("x", domain.MaxEnvironmentProgressBytes+1)} {
		if rec := w.post(t, "/of-a/builds/key-1/progress", domain.EnvironmentBuildProgress{Attempt: 1, Step: step}); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got = %d %s, want = 400", name, rec.Code, rec.Body)
		}
	}
	if want := []string{"key-1 [2/3] RUN apt-get install gcc: started"}; !slices.Equal(w.store.progress, want) {
		t.Errorf("progress = %q, want %q", w.store.progress, want)
	}
}
