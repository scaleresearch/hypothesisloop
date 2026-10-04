package envbuild

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gatewaypb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/solver/errdefs"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// buildPlatform is the platform every environment is built for: the job images are amd64.
const buildPlatform = "linux/amd64"

// Credential returns the username and password the registry of the build's repository takes,
// asked each time the daemon needs them.
type Credential = func(ctx context.Context) (username, password string, err error)

// solver is the part of a BuildKit client a build uses.
type solver interface {
	Solve(ctx context.Context, def *llb.Definition, opt client.SolveOpt, statusChan chan *client.SolveStatus) (*client.SolveResponse, error)
}

// Daemon is the BuildKit daemon environments are built on.
type Daemon struct{ solver solver }

// Dial reaches the daemon at address, a unix socket in the pod.
func Dial(ctx context.Context, address string) (*Daemon, error) {
	c, err := client.New(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("envbuild: %s: %w", address, err)
	}
	return &Daemon{solver: c}, nil
}

// Run pulls the base, then builds b and pushes it by digest, and reports how it ended under no
// attempt. A base that does not pull is the platform's fault, whatever the reason, so a registry
// outage never fails a file. Once ctx is done the report is meaningless and the caller sends
// none.
func (d *Daemon) Run(ctx context.Context, b domain.EnvironmentBuild, credential Credential) domain.EnvironmentBuildReport {
	if err := d.pull(ctx, b.Base); err != nil {
		return platformFault(err)
	}
	host, _, _ := strings.Cut(b.Repository, "/")
	attachables := []session.Attachable{&registryAuth{host: host, credential: credential}}
	digest, err := d.build(ctx, attachables, b)
	var failed *buildFailed
	switch {
	case errors.As(err, &failed):
		return domain.EnvironmentBuildReport{Outcome: domain.EnvironmentBuildFileFault, LogTail: failed.log}
	case err != nil:
		return platformFault(err)
	}
	return domain.EnvironmentBuildReport{Outcome: domain.EnvironmentBuildPushed, Digest: digest}
}

func platformFault(err error) domain.EnvironmentBuildReport {
	return domain.EnvironmentBuildReport{Outcome: domain.EnvironmentBuildPlatformFault, LogTail: lastBytes(err.Error(), domain.MaxEnvironmentLogBytes)}
}

// pull runs a command on image, which pulls and unpacks it into the daemon's cache.
func (d *Daemon) pull(ctx context.Context, image string) error {
	def, err := llb.Image(image, llb.Platform(platforms.MustParse(buildPlatform))).Run(llb.Args([]string{"true"})).Root().Marshal(ctx)
	if err != nil {
		return err
	}
	if _, err := d.solver.Solve(ctx, def, client.SolveOpt{}, nil); err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	return nil
}

// buildFailed is a build an instruction of the file failed, with the end of its output, at most
// domain.MaxEnvironmentLogBytes.
type buildFailed struct{ log string }

func (e *buildFailed) Error() string { return "the build failed" }

// build builds the file on a context holding only the file and pushes the result by digest,
// with no tag, which it returns. A *buildFailed is an instruction of the file failing; any other
// error is the platform's.
func (d *Daemon) build(ctx context.Context, attachables []session.Attachable, b domain.EnvironmentBuild) (string, error) {
	file := dockerfileState(b.Dockerfile)
	opt := client.SolveOpt{
		Frontend:       "dockerfile.v0",
		FrontendAttrs:  map[string]string{"build-arg:BASE": b.Base, "platform": buildPlatform},
		FrontendInputs: map[string]llb.State{"dockerfile": file, "context": file},
		Exports: []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{
			string(exptypes.OptKeyName): b.Repository, string(exptypes.OptKeyPush): "true", string(exptypes.OptKeyPushByDigest): "true",
		}}},
		Session: attachables,
	}

	var log strings.Builder
	var resp *client.SolveResponse
	statuses := make(chan *client.SolveStatus)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		resp, err = d.solver.Solve(groupCtx, nil, opt, statuses)
		return err
	})
	group.Go(func() error {
		for s := range statuses {
			for _, v := range s.Logs {
				log.Write(v.Data)
			}
			for _, v := range s.Vertexes {
				if v.Error != "" {
					fmt.Fprintf(&log, "\n%s: %s\n", v.Name, v.Error)
				}
			}
			if log.Len() > 4*domain.MaxEnvironmentLogBytes {
				tail := lastBytes(log.String(), domain.MaxEnvironmentLogBytes)
				log.Reset()
				log.WriteString(tail)
			}
		}
		return nil
	})
	err := group.Wait()
	if err != nil {
		// A RUN a cancel stopped exits with a code it did not choose.
		if ctx.Err() == nil && fileFault(err) {
			return "", &buildFailed{log: lastBytes(log.String()+"\n"+err.Error(), domain.MaxEnvironmentLogBytes)}
		}
		return "", fmt.Errorf("build: %w", err)
	}
	digest := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	if digest == "" {
		return "", errors.New("build: the daemon pushed and named no digest")
	}
	return digest, nil
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

// dockerfileState is the Dockerfile and the build context the frontend reads it from: one state
// holding the file alone. The daemon refuses an input with no operation, which an empty
// llb.Scratch() is.
func dockerfileState(dockerfile string) llb.State {
	return llb.Scratch().File(llb.Mkfile("Dockerfile", 0o644, []byte(dockerfile)))
}

// fileFault tells an instruction of the file failing from every other error, which is the
// platform's: a RUN that exited with a code of its own, or the frontend refusing a line of the
// file before anything ran (a parse error, a FROM it cannot resolve), which carries the line and
// no vertex. An operation that failed otherwise, a pull, the push, the daemon, carries no exit
// code or a vertex beside its line. The base itself was pulled before the build, so a FROM that
// does not resolve is a later stage of the file's own.
func fileFault(err error) bool {
	var exit *gatewaypb.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode != gatewaypb.UnknownExitStatus
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
