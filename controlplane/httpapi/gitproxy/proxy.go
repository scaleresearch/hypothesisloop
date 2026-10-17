// Package gitproxy serves git's smart HTTP protocol for the repository of each platform
// experiment, so that what the platform launches clones, fetches and pushes with its own platform
// token and never holds a credential of the git host. The proxy decides who reads and who writes,
// refuses every ref update a pusher has no right to before the host sees the push, and reaches
// the host with a credential it mints for that one request.
package gitproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Store is what the proxy reads to decide. *db.PlatformExperimentsStore with *db.EnvironmentsStore
// satisfies it.
type Store interface {
	PlatformExperimentRepository(ctx context.Context, id string) (projectID, repository string, closed, draft bool, err error)
	GetAgentSignup(ctx context.Context, platformExpID, agentID string) (*domain.AgentSignup, error)
	AgentNames(ctx context.Context, platformExpID string) (map[string]string, error)
	AssignedEnvironmentBuild(ctx context.Context, cluster, key string, attempt int) (*domain.Environment, error)
}

const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
)

// transferLimit is how long one clone, fetch or push may take through the proxy. The API server's
// own deadlines are sized for API calls, far shorter than a large pack takes.
const transferLimit = time.Hour

// Proxy serves /git/<platform experiment id>.git, and /git/builds/<key>.git to the build of an
// environment, behind auth.Gate.
type Proxy struct {
	store  Store
	host   githost.Provider
	client *http.Client
	logger *zap.Logger
}

func New(store Store, host githost.Provider, logger *zap.Logger) *Proxy {
	if store == nil || host == nil || logger == nil {
		panic("gitproxy.New: a store, a git host and a logger are required")
	}
	return &Proxy{store: store, host: host, client: &http.Client{}, logger: logger}
}

// grant is what one request may do with one repository.
type grant struct {
	principal  auth.Principal
	projectID  string
	repository string
	write      bool
	// role and agentName are the pusher's, set on a grant that writes.
	role      domain.SignupRole
	agentName string
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, _ := strings.CutPrefix(r.URL.Path, auth.GitPrefix)
	buildRest, build := strings.CutPrefix(rest, domain.BuildsGitPath+"/")
	if build {
		rest = buildRest
	}
	pe, endpoint, _ := strings.Cut(rest, "/")
	pe = strings.TrimSuffix(pe, ".git")
	var service string
	switch {
	case endpoint == "info/refs" && r.Method == http.MethodGet:
		service = r.URL.Query().Get("service")
		if service != uploadPack && service != receivePack {
			refuse(w, http.StatusForbidden, "only git's smart HTTP protocol is served: service is git-upload-pack or git-receive-pack")
			return
		}
	case (endpoint == uploadPack || endpoint == receivePack) && r.Method == http.MethodPost:
		service = endpoint
	default:
		refuse(w, http.StatusNotFound, "not found: the git proxy serves "+auth.GitPrefix+"<platform experiment id>.git/info/refs, /git-upload-pack and /git-receive-pack")
		return
	}
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(transferLimit)
	if err := errors.Join(controller.SetReadDeadline(deadline), controller.SetWriteDeadline(deadline)); err != nil {
		p.internal(w, "set the deadlines of a git transfer", err)
		return
	}
	var g *grant
	var err error
	if build {
		g, err = p.authorizeBuild(r.Context(), pe, service == receivePack)
	} else {
		g, err = p.authorize(r.Context(), pe, service == receivePack)
	}
	if err != nil {
		p.refusal(w, err)
		return
	}
	if endpoint == "info/refs" {
		p.forward(w, r, g, "/info/refs?service="+service, nil, false)
		return
	}
	if service == uploadPack {
		p.forward(w, r, g, "/"+uploadPack, r.Body, true)
		return
	}
	p.receive(w, r, g, pe)
}

// authorize says what the caller of r may do with the repository of platform experiment pe. An
// agent, a coordinator and a job of it read it while their run goes on; an agent writes once the
// platform experiment is past its draft, unless it is the draft's planner. Every other caller,
// and one of another platform experiment, finds no repository.
func (p *Proxy) authorize(ctx context.Context, pe string, write bool) (*grant, error) {
	principal, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Unauthenticated("the request carries no principal")
	}
	switch principal.Kind {
	case auth.KindAgent, auth.KindCoordinator, auth.KindJob:
	case auth.KindBuild:
		return nil, huma.Error404NotFound("no repository at this address: your build token's is " + auth.GitPrefix + domain.BuildsGitPath + "/" + principal.Build.Key + ".git")
	default:
		return nil, auth.Forbidden("a " + string(principal.Kind) + " token reaches no repository through the git proxy: it serves the agents, coordinators and jobs of a platform experiment")
	}
	if pe == "" || pe != principal.PlatformExperimentID {
		return nil, huma.Error404NotFound("no repository at this address: your token's is " + auth.GitPrefix + principal.PlatformExperimentID + ".git")
	}
	projectID, repository, closed, draft, err := p.store.PlatformExperimentRepository(ctx, pe)
	if err != nil {
		return nil, err
	}
	switch {
	case closed:
		return nil, auth.Forbidden("run closed: platform experiment " + pe + " is closed, so its repository is no longer reached through the platform. What is not pushed stays out of it")
	case principal.RunOver:
		return nil, auth.Forbidden("run over: the run of this " + string(principal.Kind) + " token in platform experiment " + pe + " has ended, so its repository is no longer reached through the platform")
	case repository == "":
		return nil, huma.Error404NotFound("platform experiment " + pe + " names no repository")
	}
	g := &grant{principal: principal, projectID: projectID, repository: repository}
	if !write {
		return g, nil
	}
	readOnly := func(why string) error {
		return auth.Forbidden("read-only: " + why + ", so nothing is pushed with it")
	}
	if principal.Kind != auth.KindAgent {
		return nil, readOnly("a " + string(principal.Kind) + " reads the repository of its platform experiment")
	}
	if draft {
		return nil, readOnly("platform experiment " + pe + " is a draft")
	}
	signup, err := p.store.GetAgentSignup(ctx, pe, principal.AgentID)
	if err != nil {
		return nil, err
	}
	if signup == nil {
		return nil, readOnly("agent " + principal.AgentID + " is not signed up to platform experiment " + pe)
	}
	if signup.Role == domain.SignupRolePlanner {
		return nil, readOnly("a planner reads the repository it plans for")
	}
	if signup.Role != domain.SignupRoleBaseline {
		names, err := p.store.AgentNames(ctx, pe)
		if err != nil {
			return nil, err
		}
		for name, agent := range names {
			if agent != principal.AgentID && domain.AgentBranchName(name) == domain.AgentBranchName(signup.Name) {
				return nil, readOnly(fmt.Sprintf("your name %q and agent %s's %q make the same branch names", signup.Name, agent, name))
			}
		}
	}
	g.write, g.role, g.agentName = true, signup.Role, signup.Name
	return g, nil
}

// authorizeBuild grants the token of an environment build a read of the repository of that
// environment, while the build is assigned and building, and nothing else.
func (p *Proxy) authorizeBuild(ctx context.Context, key string, write bool) (*grant, error) {
	principal, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Unauthenticated("the request carries no principal")
	}
	if principal.Kind != auth.KindBuild {
		return nil, auth.Forbidden("a " + string(principal.Kind) + " token reaches no environment build's repository: " + auth.GitPrefix + domain.BuildsGitPath + "/ serves environment builds alone")
	}
	if key == "" || key != principal.Build.Key {
		return nil, huma.Error404NotFound("no repository at this address: your build token's is " + auth.GitPrefix + domain.BuildsGitPath + "/" + principal.Build.Key + ".git")
	}
	if write {
		return nil, auth.Forbidden("read-only: an environment build reads the repository it is of, so nothing is pushed with its token")
	}
	env, err := p.store.AssignedEnvironmentBuild(ctx, principal.Build.Cluster, principal.Build.Key, principal.Build.Attempt)
	if err != nil {
		return nil, err
	}
	switch {
	case env == nil:
		return nil, auth.Forbidden("build over: the build of " + key + " is no longer building at this attempt, so its repository is no longer reached with its token")
	case env.GitRepository == "":
		return nil, huma.Error404NotFound("the environment " + key + " is of no repository")
	}
	return &grant{principal: principal, projectID: env.ProjectID, repository: env.GitRepository}, nil
}

// receive forwards a push whose every ref update the pusher may make, and answers any other with
// git's report of each update refused, before the host sees any of it.
func (p *Proxy) receive(w http.ResponseWriter, r *http.Request, g *grant, pe string) {
	body := io.Reader(r.Body)
	gzipped := r.Header.Get("Content-Encoding") == "gzip"
	if gzipped {
		unzipped, err := gzip.NewReader(r.Body)
		if err != nil {
			refuse(w, http.StatusBadRequest, "the push is not gzip as its Content-Encoding says")
			return
		}
		body = unzipped
	}
	commands, capabilities, raw, err := readCommands(body)
	if err != nil {
		refuse(w, http.StatusBadRequest, "the push could not be read: "+err.Error())
		return
	}
	refusals := map[string]string{}
	for _, c := range commands {
		update := domain.RefUpdate{Ref: c.Ref, Created: c.creates(), Deleted: c.deletes()}
		if why := domain.RefusedRefUpdate(pe, g.role, g.agentName, update); why != "" {
			refusals[c.Ref] = why
			p.logger.Warn("git push refused", zap.String("platformExperimentID", pe), zap.String("agentID", g.principal.AgentID),
				zap.String("ref", c.Ref), zap.Bool("delete", update.Deleted), zap.String("reason", why))
		}
	}
	if len(refusals) == 0 {
		p.forward(w, r, g, "/"+receivePack, io.MultiReader(bytes.NewReader(raw), body), !gzipped)
		return
	}
	// Read to the end, so git hears the refusal rather than a connection closed under its pack.
	if _, err := io.Copy(io.Discard, body); err != nil {
		p.logger.Warn("reading the rest of a refused push", zap.String("platformExperimentID", pe), zap.Error(err))
		return
	}
	why := func(c command) string {
		if refused, ok := refusals[c.Ref]; ok {
			return refused
		}
		return "pushed with a ref that is refused, and a push is taken whole or not at all"
	}
	message := "the platform refused this push: an agent pushes and deletes only its own branches, never the default branch or a tag\n"
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(refusalReport(commands, why, capabilities, message))
}

// forwardedRequest and forwardedResponse are the headers of git's smart HTTP that pass between
// git and the host. Authorization never does: each side has its own.
var (
	forwardedRequest  = []string{"Accept", "Accept-Encoding", "Content-Type", "Git-Protocol", "User-Agent"}
	forwardedResponse = []string{"Cache-Control", "Content-Encoding", "Content-Type", "Expires", "Pragma"}
)

// forward sends r on to the repository's address at the host plus suffix, with body, and streams
// the host's answer back. body asSent holds the bytes of r's own, which go with its length and
// encoding; any other, a gzip push read here, goes with neither.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, g *grant, suffix string, body io.Reader, asSent bool) {
	ctx := r.Context()
	credential, err := p.host.Credential(ctx, g.projectID, g.repository, g.write)
	if err != nil && !errors.Is(err, githost.ErrNoCredential) {
		p.refusal(w, err)
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, r.Method, p.host.URL(g.repository)+suffix, body)
	if err != nil {
		p.internal(w, "make the request to the git host", err)
		return
	}
	for _, name := range forwardedRequest {
		if value := r.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	if asSent {
		upstream.ContentLength = r.ContentLength
		if value := r.Header.Get("Content-Encoding"); value != "" {
			upstream.Header.Set("Content-Encoding", value)
		}
	}
	if credential != nil {
		upstream.SetBasicAuth(credential.Username, credential.Password)
	}
	resp, err := p.client.Do(upstream)
	if err != nil {
		p.logger.Warn("the git host did not answer", zap.String("repository", g.repository), zap.Error(err))
		refuse(w, http.StatusBadGateway, "the git host did not answer for the repository: try again")
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		p.logger.Warn("the git host refused a proxied request", zap.String("repository", g.repository), zap.String("path", suffix), zap.Int("status", resp.StatusCode))
		refuse(w, http.StatusBadGateway, fmt.Sprintf("the git host answered HTTP %d for the repository", resp.StatusCode))
		return
	}
	for _, name := range forwardedResponse {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if ferr := controller.Flush(); ferr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.logger.Warn("the git host's answer broke off", zap.String("repository", g.repository), zap.Error(err))
			}
			return
		}
	}
}

// refuse answers in plain text, which git prints after "remote:".
func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

// refusal answers a refusal with its status and message, and anything else as an internal error.
func (p *Proxy) refusal(w http.ResponseWriter, err error) {
	var refused interface {
		error
		GetStatus() int
	}
	if errors.As(err, &refused) {
		refuse(w, refused.GetStatus(), refused.Error())
		return
	}
	p.internal(w, "serve a git request", err)
}

func (p *Proxy) internal(w http.ResponseWriter, what string, err error) {
	p.logger.Error("git proxy: could not "+what, zap.Error(err))
	refuse(w, http.StatusInternalServerError, auth.InternalError)
}
