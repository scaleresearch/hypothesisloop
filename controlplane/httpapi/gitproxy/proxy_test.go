package gitproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/middleware"
)

const (
	hostUser     = "platform"
	hostPassword = "host-password-no-agent-may-see"
	repository   = "org-1/model.git"
)

type fakeStore struct {
	closed, draft bool
	signups       map[string]domain.AgentSignup
	// elsewhere, when set, is the repository the platform experiment names in place of its own.
	elsewhere string
}

func (s *fakeStore) PlatformExperimentRepository(_ context.Context, id string) (string, string, bool, bool, error) {
	if s.elsewhere != "" {
		return "prj-1", s.elsewhere, s.closed, s.draft, nil
	}
	return "prj-1", repository, s.closed, s.draft, nil
}

// projects is prj-1 of org-1.
type projects struct{}

func (projects) GetProject(_ context.Context, id string) (*domain.Project, error) {
	if id != "prj-1" {
		return nil, nil
	}
	return &domain.Project{ID: id, OrgID: "org-1"}, nil
}

func (s *fakeStore) GetAgentSignup(_ context.Context, _, agentID string) (*domain.AgentSignup, error) {
	signup, ok := s.signups[agentID]
	if !ok {
		return nil, nil
	}
	return &signup, nil
}

func (s *fakeStore) AgentNames(context.Context, string) (map[string]string, error) {
	names := map[string]string{}
	for id, signup := range s.signups {
		names[signup.Name] = id
	}
	return names, nil
}

type tokens map[string]auth.Principal

func (t tokens) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	if p, ok := t[token]; ok {
		return p, nil
	}
	return auth.Principal{}, auth.Unauthenticated("unknown token")
}

var principals = tokens{
	"tok-executor-2":  {Kind: auth.KindAgent, AgentID: "agent-r2", PlatformExperimentID: "pe-1"},
	"tok-executor-3":  {Kind: auth.KindAgent, AgentID: "agent-r3", PlatformExperimentID: "pe-1"},
	"tok-baseline":    {Kind: auth.KindAgent, AgentID: "agent-b1", PlatformExperimentID: "pe-1"},
	"tok-planner":     {Kind: auth.KindAgent, AgentID: "agent-p1", PlatformExperimentID: "pe-1"},
	"tok-researcher":  {Kind: auth.KindAgent, AgentID: "agent-s1", PlatformExperimentID: "pe-1"},
	"tok-coordinator": {Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1"},
	"tok-job":         {Kind: auth.KindJob, AgentID: "agent-r2", ExperimentID: "job-1", PlatformExperimentID: "pe-1"},
	"tok-other-pe":    {Kind: auth.KindAgent, AgentID: "agent-x", PlatformExperimentID: "pe-2"},
	"tok-over":        {Kind: auth.KindAgent, AgentID: "agent-r2", PlatformExperimentID: "pe-1", RunOver: true},
	"tok-staff":       {Kind: auth.KindStaff},
}

func signups() map[string]domain.AgentSignup {
	return map[string]domain.AgentSignup{
		"agent-r2": {AgentID: "agent-r2", Name: "Executor 2", Role: domain.SignupRoleCompetitor},
		"agent-r3": {AgentID: "agent-r3", Name: "Executor 3", Role: domain.SignupRoleCompetitor},
		"agent-b1": {AgentID: "agent-b1", Name: "Baseline 1", Role: domain.SignupRoleBaseline},
		"agent-p1": {AgentID: "agent-p1", Name: "Planner 1", Role: domain.SignupRolePlanner},
		"agent-s1": {AgentID: "agent-s1", Name: "Researcher 1", Role: domain.SignupRoleResearcher},
	}
}

type world struct {
	t     *testing.T
	root  string
	bare  string
	proxy string
	// proxyBase is where the proxy is served, hostURL the repository's address at the host.
	proxyBase string
	hostURL   string
	store     *fakeStore
	logs      *observer.ObservedLogs
	outputs   []string
}

func gitBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	return path
}

// newWorld is a bare repository served by git http-backend behind the host's one credential, as
// a plain host serves it, and the proxy in front of it behind the gate, as the API serves it.
func newWorld(t *testing.T) *world {
	t.Helper()
	binary := gitBinary(t)
	w := &world{t: t, root: t.TempDir(), store: &fakeStore{signups: signups()}}
	w.bare = filepath.Join(w.root, "host", repository)
	w.run("", "init", "--quiet", "--bare", "-b", "main", w.bare)
	w.run(w.bare, "config", "http.receivepack", "true")
	seed := filepath.Join(w.root, "seed")
	w.run("", "init", "--quiet", "-b", "main", seed)
	w.run(seed, "commit", "--quiet", "--allow-empty", "-m", "Start the repository")
	w.run(seed, "push", "--quiet", w.bare, "main")

	backend := &cgi.Handler{Path: binary, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(w.root, "host"), "GIT_HTTP_EXPORT_ALL=1"}}
	host := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != hostUser || password != hostPassword {
			rw.Header().Set("WWW-Authenticate", `Basic realm="host"`)
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(rw, r)
	}))
	t.Cleanup(host.Close)
	plain, err := githost.NewPlain(host.URL, hostUser, hostPassword, "experiment-template", projects{})
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	w.logs = logs
	logger := zap.New(core)
	handler := middleware.Logging(logger)(auth.Gate(principals, logger, nil)(New(w.store, plain, logger)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	w.proxyBase = server.URL + "/git"
	w.proxy = domain.GitProxyURL(w.proxyBase, "pe-1")
	w.hostURL = plain.URL(repository)
	return w
}

func (w *world) env(token string) []string {
	home := filepath.Join(w.root, "home")
	_ = os.MkdirAll(home, 0o700)
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid"}
	if token != "" {
		// The caller's own platform token, as the Bearer header agents send with plain git.
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Bearer "+token)
	}
	return env
}

func (w *world) git(token, dir string, args ...string) (string, error) {
	w.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = w.env(token)
	out, err := cmd.CombinedOutput()
	w.outputs = append(w.outputs, string(out))
	return string(out), err
}

func (w *world) run(dir string, args ...string) string {
	w.t.Helper()
	out, err := w.git("", dir, args...)
	if err != nil {
		w.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(out)
}

func (w *world) clone(token, name string) string {
	w.t.Helper()
	dir := filepath.Join(w.root, name)
	if out, err := w.git(token, w.root, "clone", "--quiet", w.proxy, dir); err != nil {
		w.t.Fatalf("clone as %s: %v: %s", token, err, out)
	}
	return dir
}

func (w *world) hostRef(ref string) string {
	w.t.Helper()
	out, _ := w.git("", w.bare, "rev-parse", "--verify", "--quiet", ref)
	return strings.TrimSpace(out)
}

// commitOn makes a new commit on branch in the clone at dir.
func (w *world) commitOn(dir, branch string) string {
	w.t.Helper()
	w.run(dir, "checkout", "--quiet", "-B", branch)
	w.run(dir, "commit", "--quiet", "--allow-empty", "-m", "work on "+branch)
	return w.run(dir, "rev-parse", "HEAD")
}

// noSecretLeaked checks that neither the host's credential nor a platform token is in anything git
// printed or the proxy logged.
func (w *world) noSecretLeaked() {
	w.t.Helper()
	secrets := []string{hostPassword}
	for token := range principals {
		secrets = append(secrets, token)
	}
	var logged strings.Builder
	for _, entry := range w.logs.All() {
		fmt.Fprintln(&logged, entry.Message, entry.ContextMap())
	}
	if len(w.logs.All()) == 0 {
		w.t.Fatal("the proxy logged nothing, so nothing is checked")
	}
	for _, secret := range secrets {
		if strings.Contains(logged.String(), secret) {
			w.t.Errorf("the proxy logged %q", secret)
		}
		for _, out := range w.outputs {
			if strings.Contains(out, secret) {
				w.t.Errorf("git printed %q: %s", secret, out)
			}
		}
	}
}

func TestAnExecutorPushesItsOwnBranchAndNothingElseThroughTheProxy(t *testing.T) {
	w := newWorld(t)
	dir := w.clone("tok-executor-2", "executor-2")
	main := w.hostRef("refs/heads/main")

	own := "hl/pe-1/executor-2/faster-attention-abc123"
	head := w.commitOn(dir, own)
	if out, err := w.git("tok-executor-2", dir, "push", "--quiet", "origin", own); err != nil {
		t.Fatalf("pushing its own branch: %v: %s", err, out)
	}
	if got := w.hostRef("refs/heads/" + own); got != head {
		t.Fatalf("the host holds %q on %s, want %s", got, own, head)
	}
	head = w.commitOn(dir, own)
	if out, err := w.git("tok-executor-2", dir, "push", "--quiet", "origin", own); err != nil || w.hostRef("refs/heads/"+own) != head {
		t.Fatalf("pushing more onto its own branch: %v: %s", err, out)
	}
	theirs := "hl/pe-1/executor-3/y"
	otherDir := w.clone("tok-executor-3", "executor-3")
	w.commitOn(otherDir, theirs)
	if out, err := w.git("tok-executor-3", otherDir, "push", "--quiet", "origin", theirs); err != nil {
		t.Fatalf("another agent pushing its own branch: %v: %s", err, out)
	}

	for name, c := range map[string]struct {
		args []string
		ref  string
		why  string
	}{
		"the default branch":               {[]string{"push", "origin", "HEAD:refs/heads/main"}, "refs/heads/main", "your branches are under hl/pe-1/executor-2/"},
		"another agent's branch":           {[]string{"push", "origin", "HEAD:refs/heads/hl/pe-1/executor-3/x"}, "refs/heads/hl/pe-1/executor-3/x", "your branches are under hl/pe-1/executor-2/"},
		"a force push to another's branch": {[]string{"push", "--force", "origin", "HEAD:refs/heads/" + theirs}, "refs/heads/" + theirs, "your branches are under hl/pe-1/executor-2/"},
		"a delete of another's branch":     {[]string{"push", "origin", ":refs/heads/" + theirs}, "refs/heads/" + theirs, "your branches are under hl/pe-1/executor-2/"},
		"a delete of the default branch":   {[]string{"push", "origin", ":refs/heads/main"}, "refs/heads/main", "your branches are under hl/pe-1/executor-2/"},
		"a tag":                            {[]string{"push", "origin", "HEAD:refs/tags/v1"}, "refs/tags/v1", "only branches are pushed"},
		"a force push to main":             {[]string{"push", "--force", "origin", "HEAD:refs/heads/main"}, "refs/heads/main", "your branches are under hl/pe-1/executor-2/"},
	} {
		before := w.hostRef(c.ref)
		out, err := w.git("tok-executor-2", dir, c.args...)
		if err == nil || !strings.Contains(out, "[remote rejected]") || !strings.Contains(out, c.why) {
			t.Errorf("%s: %v: %s, want it rejected saying %q", name, err, out, c.why)
		}
		if after := w.hostRef(c.ref); after != before {
			t.Errorf("%s: the host's %s moved from %q to %q", name, c.ref, before, after)
		}
	}
	if w.hostRef("refs/heads/main") != main {
		t.Error("main moved")
	}
	refused := w.logs.FilterMessage("git push refused").All()
	if len(refused) < 7 || refused[0].ContextMap()["agentID"] != "agent-r2" || refused[0].ContextMap()["ref"] == "" {
		t.Errorf("refused pushes logged = %+v, want each with the agent and the ref", refused)
	}

	// One refused ref refuses the push it is in whole.
	other := w.commitOn(dir, own)
	out, err := w.git("tok-executor-2", dir, "push", "origin", own, "HEAD:refs/heads/main")
	if err == nil || !strings.Contains(out, "a push is taken whole") || w.hostRef("refs/heads/"+own) == other {
		t.Errorf("a push of its own branch beside main: %v: %s, want both refused", err, out)
	}

	// Its own branch it deletes, and the other agent's stays.
	if out, err := w.git("tok-executor-2", dir, "push", "--quiet", "origin", ":refs/heads/"+own); err != nil || w.hostRef("refs/heads/"+own) != "" {
		t.Errorf("deleting its own branch: %v: %s", err, out)
	}
	if w.hostRef("refs/heads/"+theirs) == "" {
		t.Error("the other agent's branch is gone")
	}
	w.noSecretLeaked()
}

func TestTheBaselineCreatesNoDefaultBranchOnAnEmptyRepository(t *testing.T) {
	w := newWorld(t)
	empty := filepath.Join(w.root, "host", repository)
	if err := os.RemoveAll(empty); err != nil {
		t.Fatal(err)
	}
	w.run("", "init", "--quiet", "--bare", "-b", "main", empty)
	w.run(empty, "config", "http.receivepack", "true")
	dir := w.clone("tok-baseline", "baseline")
	w.run(dir, "commit", "--quiet", "--allow-empty", "-m", "Start the repository")
	if out, err := w.git("tok-baseline", dir, "push", "origin", "HEAD:refs/heads/main"); err == nil || !strings.Contains(out, "[remote rejected]") {
		t.Errorf("creating main on an empty repository: %v: %s, want it rejected", err, out)
	}
	if w.hostRef("refs/heads/main") != "" {
		t.Error("main was created")
	}
	w.commitOn(dir, "hl/pe-1/baseline")
	if out, err := w.git("tok-baseline", dir, "push", "--quiet", "origin", "hl/pe-1/baseline"); err != nil {
		t.Fatalf("pushing the baseline branch: %v: %s", err, out)
	}
	w.noSecretLeaked()
}

func TestOnlyAnAgentPastTheDraftPushes(t *testing.T) {
	w := newWorld(t)
	for _, token := range []string{"tok-planner", "tok-researcher", "tok-coordinator", "tok-job"} {
		dir := w.clone(token, strings.TrimPrefix(token, "tok-"))
		w.commitOn(dir, "hl/pe-1/executor-2/x")
		out, err := w.git(token, dir, "push", "origin", "hl/pe-1/executor-2/x")
		if err == nil || !strings.Contains(out, "read-only") {
			t.Errorf("%s pushing: %v: %s, want it refused as read-only", token, err, out)
		}
	}
	w.store.draft = true
	dir := w.clone("tok-executor-2", "in-draft")
	w.commitOn(dir, "hl/pe-1/executor-2/x")
	if out, err := w.git("tok-executor-2", dir, "push", "origin", "hl/pe-1/executor-2/x"); err == nil || !strings.Contains(out, "is a draft") {
		t.Errorf("an executor pushing in a draft: %v: %s, want it refused", err, out)
	}
	if w.hostRef("refs/heads/hl/pe-1/executor-2/x") != "" {
		t.Error("a read-only push reached the host")
	}
	w.noSecretLeaked()
}

func TestTheProxyServesNoOneElse(t *testing.T) {
	w := newWorld(t)
	get := func(token, path string) (int, string, http.Header) {
		request, err := http.NewRequest(http.MethodGet, w.proxy+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), resp.Header
	}
	status, body, header := get("tok-executor-2", "/info/refs?service=git-upload-pack")
	if status != http.StatusOK || header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" || !strings.Contains(body, "refs/heads/main") {
		t.Fatalf("an agent's ref listing: %d %s", status, body)
	}
	for name, c := range map[string]struct {
		token, path string
		status      int
	}{
		"no token":                          {"", "/info/refs?service=git-upload-pack", http.StatusUnauthorized},
		"staff":                             {"tok-staff", "/info/refs?service=git-upload-pack", http.StatusForbidden},
		"another platform experiment's":     {"tok-other-pe", "/info/refs?service=git-upload-pack", http.StatusNotFound},
		"a run that is over":                {"tok-over", "/info/refs?service=git-upload-pack", http.StatusForbidden},
		"the dumb protocol":                 {"tok-executor-2", "/info/refs", http.StatusForbidden},
		"a file of the repository":          {"tok-executor-2", "/HEAD", http.StatusNotFound},
		"a job listing refs to push":        {"tok-job", "/info/refs?service=git-receive-pack", http.StatusForbidden},
		"a planner listing refs to push":    {"tok-planner", "/info/refs?service=git-receive-pack", http.StatusForbidden},
		"a researcher listing refs to push": {"tok-researcher", "/info/refs?service=git-receive-pack", http.StatusForbidden},
	} {
		status, body, header := get(c.token, c.path)
		if status != c.status {
			t.Errorf("%s: got = %d %s, want %d", name, status, body, c.status)
		}
		if strings.Contains(body, hostPassword) || strings.Contains(header.Get("WWW-Authenticate"), "host") {
			t.Errorf("%s: the host's credential or challenge reached the caller: %v %s", name, header, body)
		}
	}
	w.store.closed = true
	if status, body, _ := get("tok-executor-2", "/info/refs?service=git-upload-pack"); status != http.StatusForbidden || !strings.Contains(body, "run closed") {
		t.Errorf("a closed platform experiment: %d %s, want 403 run closed", status, body)
	}
	w.noSecretLeaked()
}

func TestAnAgentWhoseNameMakesAnotherAgentsBranchNamesPushesNothing(t *testing.T) {
	w := newWorld(t)
	w.store.signups["agent-r3"] = domain.AgentSignup{AgentID: "agent-r3", Name: "executor-2", Role: domain.SignupRoleCompetitor}
	dir := w.clone("tok-executor-2", "executor-2")
	w.commitOn(dir, "hl/pe-1/executor-2/x")
	if out, err := w.git("tok-executor-2", dir, "push", "origin", "hl/pe-1/executor-2/x"); err == nil || !strings.Contains(out, "make the same branch names") {
		t.Errorf("pushing beside an agent of the same branch names: %v: %s, want it refused", err, out)
	}
}

// A platform experiment that names a repository outside its org's directory on the host, as one
// of another org, is served nothing: the host's credential is never minted for it.
func TestARepositoryOfAnotherOrgIsNotServed(t *testing.T) {
	w := newWorld(t)
	other := filepath.Join(w.root, "host", "org-2", "model.git")
	w.run("", "init", "--quiet", "--bare", "-b", "main", other)
	for _, elsewhere := range []string{"org-2/model.git", "model.git"} {
		w.store.elsewhere = elsewhere
		if out, err := w.git("tok-executor-2", w.root, "ls-remote", w.proxy); err == nil {
			t.Errorf("%s was served through the proxy: %s", elsewhere, out)
		}
	}
}
