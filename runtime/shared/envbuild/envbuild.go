package envbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gatewaypb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/solver/errdefs"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// buildPlatform is the platform every environment is built for: the job images are amd64.
const buildPlatform = "linux/amd64"

// Credential returns the username and password the registry of the build's repository takes,
// asked each time the daemon needs them.
type Credential = func(ctx context.Context) (username, password string, err error)

// GitCredential returns what reads the repository the build's file is of, asked each time a RUN
// mounts clusterapi.EnvironmentGitSecret.
type GitCredential = func(ctx context.Context) (clusterapi.EnvironmentGitCredential, error)

// solver is the part of a BuildKit client a build uses.
type solver interface {
	Solve(ctx context.Context, def *llb.Definition, opt client.SolveOpt, statusChan chan *client.SolveStatus) (*client.SolveResponse, error)
}

// Daemon is the BuildKit daemon environments are built on.
type Daemon struct {
	solver solver
	loader Loader
	node   string
}

// Loader puts each image a build pushes into the image store of a node that runs its jobs, so a
// job there starts without pulling back what was just pushed. It reads the image as an OCI layout
// tar while the daemon exports it, names it repository@digest, and returns that digest. The
// daemon pushes only once the tar is read to its end, so Load reads it all before anything slow.
type Loader interface {
	Load(ctx context.Context, repository string, image io.Reader) (digest string, err error)
}

// LoadInto has every build after it also load its image with loader into the node named node. A
// load that fails costs the job a pull and never fails the build. With node named, a load that
// ends is told as it ends, while the push goes on, so a job placed on that node starts first.
func (d *Daemon) LoadInto(loader Loader, node string) { d.loader, d.node = loader, node }

// Loaded is told the node a build's image was loaded into and its digest, once, before the build
// ends.
type Loaded = func(node, digest string)

// Dial reaches the daemon at address, a unix socket in the pod.
func Dial(ctx context.Context, address string) (*Daemon, error) {
	c, err := client.New(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("envbuild: %s: %w", address, err)
	}
	return &Daemon{solver: c}, nil
}

// Progress is told each step of a build as it starts and ends, and how far its transfers are.
type Progress = func(format string, args ...any)

// Run pulls the base and the image of the platform's tools, then builds b and pushes it by
// digest, and reports how it ended under no attempt. An image of the platform's that does not
// pull is the platform's fault, whatever the reason, so a registry outage never fails a file. Once ctx is done the report is meaningless and the caller sends
// none. loaded, which may be nil, is told the image once it is in its node, before the push ends.
// gitCredential is what a RUN that mounts clusterapi.EnvironmentGitSecret reads the repository with.
func (d *Daemon) Run(ctx context.Context, b clusterapi.EnvironmentBuild, credential Credential, gitCredential GitCredential, progress Progress, loaded Loaded) clusterapi.EnvironmentBuildReport {
	steps := newSteps(progress)
	for _, image := range []string{b.Base, b.Tools} {
		if image == "" {
			continue
		}
		if err := d.pull(ctx, image, steps); err != nil {
			return platformFault(err)
		}
	}
	host, _, _ := strings.Cut(b.Repository, "/")
	attachables := []session.Attachable{&registryAuth{host: host, credential: credential},
		secretsprovider.NewSecretProvider(repositorySecret(gitCredential))}
	digest, err := d.build(ctx, attachables, b, steps, loaded)
	var failed *buildFailed
	switch {
	case errors.As(err, &failed):
		return clusterapi.EnvironmentBuildReport{Outcome: clusterapi.EnvironmentBuildFileFault, LogTail: failed.log}
	case err != nil:
		return platformFault(err)
	}
	return clusterapi.EnvironmentBuildReport{Outcome: clusterapi.EnvironmentBuildPushed, Digest: digest}
}

func platformFault(err error) clusterapi.EnvironmentBuildReport {
	return clusterapi.EnvironmentBuildReport{Outcome: clusterapi.EnvironmentBuildPlatformFault, LogTail: lastBytes(err.Error(), clusterapi.MaxEnvironmentLogBytes)}
}

// pull runs a command on image, which pulls and unpacks it into the daemon's cache.
func (d *Daemon) pull(ctx context.Context, image string, steps *steps) error {
	def, err := llb.Image(image, llb.Platform(platforms.MustParse(buildPlatform))).Run(llb.Args([]string{"true"})).Root().Marshal(ctx)
	if err != nil {
		return err
	}
	if _, err := d.solve(ctx, def, client.SolveOpt{}, steps.follow); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// solve runs one solve on the daemon and hands each status it streams to onStatus.
func (d *Daemon) solve(ctx context.Context, def *llb.Definition, opt client.SolveOpt, onStatus func(*client.SolveStatus)) (*client.SolveResponse, error) {
	var resp *client.SolveResponse
	statuses := make(chan *client.SolveStatus)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		resp, err = d.solver.Solve(groupCtx, def, opt, statuses)
		return err
	})
	group.Go(func() error {
		for s := range statuses {
			onStatus(s)
		}
		return nil
	})
	return resp, group.Wait()
}

// buildFailed is a build an instruction of the file failed, with the end of its output, at most
// clusterapi.MaxEnvironmentLogBytes.
type buildFailed struct{ log string }

func (e *buildFailed) Error() string { return "the build failed" }

// pushRetries are the waits before each push of a build again after its push failed with its
// image already in its node, where a job may run it. The daemon's cache holds the build, so each
// is a push alone.
var pushRetries = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute}

// build builds the file on a context holding only the file and pushes the result by digest,
// with no tag, which it returns. A *buildFailed is an instruction of the file failing; any other
// error is the platform's. A push that fails once the image is loaded into its node is tried
// again (pushRetries): a job there may already run it.
func (d *Daemon) build(ctx context.Context, attachables []session.Attachable, b clusterapi.EnvironmentBuild, steps *steps, onLoaded Loaded) (string, error) {
	if b.CacheScope == "" {
		return "", errors.New("build: the build names no cache scope")
	}
	file := dockerfileState(withTools(b.Dockerfile, b.Tools))
	// Media types are set, never the daemon's default, so the pushed manifest and the loaded one
	// are the same bytes and so the same digest.
	push := client.ExportEntry{Type: client.ExporterImage, Attrs: map[string]string{
		string(exptypes.OptKeyName): b.Repository, string(exptypes.OptKeyPush): "true", string(exptypes.OptKeyPushByDigest): "true",
		string(exptypes.OptKeyOCITypes): "true",
	}}
	// Every RUN's cache key holds its sandbox's host name, and every cache mount's id the
	// namespace, so the scope keeps both to the builds that may read what they read.
	opt := client.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{"build-arg:BASE": b.Base, "platform": buildPlatform,
			"hostname": b.CacheScope, "build-arg:BUILDKIT_CACHE_MOUNT_NS": b.CacheScope},
		FrontendInputs: map[string]llb.State{"dockerfile": file, "context": file},
		Exports:        []client.ExportEntry{push},
		Session:        attachables,
	}
	var load *loading
	if d.loader != nil {
		var told func(digest string)
		if d.node != "" && onLoaded != nil {
			told = func(digest string) {
				steps.progress("loaded into node %s as %s, pushing", d.node, digest)
				onLoaded(d.node, digest)
			}
		}
		load = newLoading(ctx, d.loader, b.Repository, told)
		// The push last: both exporters answer a digest, and the last one's is the answer.
		opt.Exports = []client.ExportEntry{{Type: client.ExporterOCI, Attrs: map[string]string{
			string(exptypes.OptKeyOCITypes): "true",
		}, Output: load.output}, push}
	}

	var log strings.Builder
	resp, err := d.solve(ctx, nil, opt, func(s *client.SolveStatus) {
		steps.follow(s)
		for _, v := range s.Logs {
			log.Write(v.Data)
		}
		for _, v := range s.Vertexes {
			if v.Error != "" {
				fmt.Fprintf(&log, "\n%s: %s\n", v.Name, v.Error)
			}
		}
		if log.Len() > 4*clusterapi.MaxEnvironmentLogBytes {
			tail := lastBytes(log.String(), clusterapi.MaxEnvironmentLogBytes)
			log.Reset()
			log.WriteString(tail)
		}
	})
	var loaded string
	var loadErr error
	if load != nil {
		loaded, loadErr = load.wait()
	}
	if err != nil {
		// A RUN a cancel stopped exits with a code it did not choose.
		if ctx.Err() == nil && fileFault(err) {
			return "", &buildFailed{log: lastBytes(log.String()+"\n"+err.Error(), clusterapi.MaxEnvironmentLogBytes)}
		}
		if load == nil || loadErr != nil || loaded == "" || ctx.Err() != nil {
			return "", fmt.Errorf("build: %w", err)
		}
		opt.Exports = []client.ExportEntry{push}
		return d.pushAgain(ctx, opt, steps, err)
	}
	digest := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	if digest == "" {
		return "", errors.New("build: the daemon pushed and named no digest")
	}
	if load != nil {
		switch {
		case loadErr != nil && loaded == digest:
			steps.progress("loading into the node: imported, then %v", loadErr)
		case loadErr != nil:
			steps.progress("loading into the node: %v; its jobs pull it", loadErr)
		case loaded != digest:
			steps.progress("loading into the node: it holds %s, the push is %s; its jobs pull it", loaded, digest)
		default:
			steps.progress("loading into the node: done, its jobs start without a pull")
		}
	}
	return digest, nil
}

// pushAgain solves opt, which pushes and loads nothing, after each wait of pushRetries until one
// push names its digest, and returns the last error when none does.
func (d *Daemon) pushAgain(ctx context.Context, opt client.SolveOpt, steps *steps, failed error) (string, error) {
	for i, wait := range pushRetries {
		steps.progress("pushing: %v; the image is in its node, pushing again in %s (%d of %d)", failed, wait, i+1, len(pushRetries))
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("build: %w", failed)
		case <-time.After(wait):
		}
		resp, err := d.solve(ctx, nil, opt, steps.follow)
		if err == nil {
			if digest := resp.ExporterResponse[exptypes.ExporterImageDigestKey]; digest != "" {
				return digest, nil
			}
			err = errors.New("the daemon pushed and named no digest")
		}
		failed = err
	}
	return "", fmt.Errorf("build: pushed %d more times: %w", len(pushRetries), failed)
}

// loadGrace is how long a build that has pushed waits for its load to finish. The load is the
// node's to unpack; a job that starts before it ends unpacks the rest itself.
const loadGrace = 2 * time.Minute

// loading streams the OCI exporter's tar into a Loader. Its writer never fails, whatever the
// loader does, so a load that breaks never fails the build: what the loader leaves unread is
// drained.
type loading struct {
	ctx        context.Context
	cancel     context.CancelFunc
	loader     Loader
	repository string
	// told, when set, is told the digest of a load that ended with no error, before done closes.
	told   func(digest string)
	once   sync.Once
	writer *io.PipeWriter
	done   chan struct{}
	digest string
	err    error
}

func newLoading(ctx context.Context, loader Loader, repository string, told func(digest string)) *loading {
	ctx, cancel := context.WithCancel(ctx)
	return &loading{ctx: ctx, cancel: cancel, loader: loader, repository: repository, told: told, done: make(chan struct{})}
}

func (l *loading) output(map[string]string) (io.WriteCloser, error) {
	started := false
	l.once.Do(func() {
		started = true
		reader, writer := io.Pipe()
		l.writer = writer
		go func() {
			defer close(l.done)
			l.digest, l.err = l.loader.Load(l.ctx, l.repository, reader)
			_, _ = io.Copy(io.Discard, reader)
			if l.err == nil && l.digest != "" && l.told != nil {
				l.told(l.digest)
			}
		}()
	})
	if !started {
		return nil, errors.New("the image is loaded once")
	}
	return l.writer, nil
}

// wait ends the stream, if the export left it open, and waits up to loadGrace for the loader.
func (l *loading) wait() (string, error) {
	defer l.cancel()
	started := true
	l.once.Do(func() { started = false })
	if !started {
		return "", errors.New("the daemon exported no image to load")
	}
	_ = l.writer.CloseWithError(errors.New("the build ended before the image was sent"))
	select {
	case <-l.done:
		return l.digest, l.err
	case <-time.After(loadGrace):
		l.cancel()
		<-l.done
		return "", fmt.Errorf("not done %s after the push", loadGrace)
	}
}

// registryAuth hands the daemon the credential of the one registry it pushes to and is no token
// authority, so the daemon fetches the registry's token itself. With the client as the authority
// (authprovider's default), every push to Artifact Registry through a v0.33 daemon waited out the
// daemon's 60 s session timeout and failed to authorize.
type registryAuth struct {
	auth.UnimplementedAuthServer
	host       string
	credential Credential
}

func (a *registryAuth) Register(server *grpc.Server) { auth.RegisterAuthServer(server, a) }

func (a *registryAuth) Credentials(ctx context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	if req.Host != a.host {
		return &auth.CredentialsResponse{}, nil
	}
	username, password, err := a.credential(ctx)
	if err != nil {
		return nil, err
	}
	return &auth.CredentialsResponse{Username: username, Secret: password}, nil
}

// repositorySecret answers clusterapi.EnvironmentGitSecret, and no other secret, with git's credential
// store for the repository the build's file is of: a line of its host alone, which a git that
// sends no path matches, then a line of its path with and without .git, which a git with
// credential.useHttpPath (the job images set it) matches as cloned. The credential reads that one
// repository alone. A repository read with no credential has no secret, which a RUN mounts as
// nothing and reads it anonymously.
type repositorySecret GitCredential

func (r repositorySecret) GetSecret(ctx context.Context, id string) ([]byte, error) {
	if id != clusterapi.EnvironmentGitSecret || r == nil {
		return nil, secrets.ErrNotFound
	}
	credential, err := r(ctx)
	if errors.Is(err, agentexec.ErrNoEnvironmentGitCredential) {
		return nil, secrets.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("the credential of the repository: %w", err)
	}
	address, err := url.Parse(credential.URL)
	if err != nil || address.Scheme == "" || address.Host == "" {
		return nil, fmt.Errorf("the repository's address %q names no scheme and host", credential.URL)
	}
	origin := address.Scheme + "://" + url.UserPassword(credential.Username, credential.Password).String() + "@" + address.Host
	path := strings.TrimSuffix(strings.Trim(address.Path, "/"), ".git")
	return []byte(origin + "\n" + origin + "/" + path + "\n" + origin + "/" + path + ".git\n"), nil
}

// withTools is dockerfile with clusterapi.PlatformTools copied from the image tools into the stage
// it builds, its last, as its last instruction, so they are the image's last layer and a new
// release of them leaves every layer before cached. The file's own lines keep their numbers.
func withTools(dockerfile, tools string) string {
	if tools == "" {
		return dockerfile
	}
	if dockerfile != "" && !strings.HasSuffix(dockerfile, "\n") {
		dockerfile += "\n"
	}
	return dockerfile + "COPY --from=" + tools + " " + strings.Join(clusterapi.PlatformTools, " ") + " /usr/local/bin/\n"
}

// dockerfileState is the Dockerfile and the build context the frontend reads it from: one state
// holding the file alone. The daemon refuses an input with no operation, which an empty
// llb.Scratch() is.
func dockerfileState(dockerfile string) llb.State {
	return llb.Scratch().File(llb.Mkfile("Dockerfile", 0o644, []byte(dockerfile)))
}

// fileFault tells an error the file's content caused from every other error, which is the
// platform's: a RUN that exited with a code of its own; a COPY or ADD whose source is not there,
// which fails computing its content's checksum or in its file operation; or the frontend refusing
// a line of the file before anything ran (a parse error, a FROM it cannot resolve), which carries
// the line and no vertex. An operation that failed otherwise, a pull, the push, the daemon,
// carries no exit code, no such subject, or a vertex beside its line. The base itself was pulled
// before the build, so a FROM that does not resolve is a later stage of the file's own.
func fileFault(err error) bool {
	var exit *gatewaypb.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode != gatewaypb.UnknownExitStatus
	}
	var solve *errdefs.SolveError
	if errors.As(err, &solve) {
		switch solve.Subject.(type) {
		case *errdefs.Solve_Cache, *errdefs.Solve_File:
			return true
		}
	}
	var line *errdefs.SourceError
	var vertex *errdefs.VertexError
	return errors.As(err, &line) && !errors.As(err, &vertex)
}

// lastBytes keeps the end of text, from the start of a line where one starts inside the limit.
func lastBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	if newline := strings.IndexByte(text, '\n'); newline >= 0 && newline < len(text)-1 {
		text = text[newline+1:]
	}
	return strings.ToValidUTF8(text, "")
}
