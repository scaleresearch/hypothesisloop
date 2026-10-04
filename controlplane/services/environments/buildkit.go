package environments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gatewaypb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/solver/errdefs"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// BuildFailed is a build the file failed, with the end of its output, at most
// domain.MaxEnvironmentLogBytes.
type BuildFailed struct{ Log string }

func (e *BuildFailed) Error() string { return "the build failed" }

// buildPlatform is the platform every environment is built for: the job images are amd64.
const buildPlatform = "linux/amd64"

// Pull runs a command on image on the daemon c reaches, which pulls and unpacks it for that, so
// its layers stay in the daemon's cache.
func Pull(ctx context.Context, c *client.Client, attachables []session.Attachable, image string) error {
	def, err := llb.Image(image, llb.Platform(platforms.MustParse(buildPlatform))).Run(llb.Args([]string{"true"})).Root().Marshal(ctx)
	if err != nil {
		return err
	}
	if _, err := c.Solve(ctx, def, client.SolveOpt{Session: attachables}, nil); err != nil {
		return fmt.Errorf("environments: pull %s: %w", image, err)
	}
	return nil
}

// Build builds dockerfile with BASE set to base, on a context holding only the file, on the
// daemon c reaches, and pushes the result as image. A *BuildFailed is an instruction of the file
// failing; any other error is the platform's.
func Build(ctx context.Context, c *client.Client, attachables []session.Attachable, dockerfile, base, image string) error {
	file := dockerfileState(dockerfile)
	opt := client.SolveOpt{
		Frontend:       "dockerfile.v0",
		FrontendAttrs:  map[string]string{"build-arg:BASE": base, "platform": buildPlatform},
		FrontendInputs: map[string]llb.State{"dockerfile": file, "context": file},
		Exports:        []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{"name": image, "push": "true"}}},
		Session:        attachables,
	}

	var log strings.Builder
	statuses := make(chan *client.SolveStatus)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		_, err := c.Solve(groupCtx, nil, opt, statuses)
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
	if err == nil {
		return nil
	}
	// A RUN the deadline or a shutdown stopped exits with a code it did not choose.
	if ctx.Err() == nil && fileFault(err) {
		return &BuildFailed{Log: lastBytes(log.String()+"\n"+err.Error(), domain.MaxEnvironmentLogBytes)}
	}
	return fmt.Errorf("environments: build: %w", err)
}

// registryAuth hands the daemon the credential of the one registry it pushes to and is no token
// authority, so the daemon fetches the registry's token itself. With the client as the authority
// (authprovider's default), every push to Artifact Registry through a v0.33 daemon waited out the
// daemon's 60 s session timeout and failed to authorize.
type registryAuth struct {
	auth.UnimplementedAuthServer
	host, username, password string
}

func (a *registryAuth) Register(server *grpc.Server) { auth.RegisterAuthServer(server, a) }

func (a *registryAuth) Credentials(_ context.Context, req *auth.CredentialsRequest) (*auth.CredentialsResponse, error) {
	if req.Host != a.host {
		return &auth.CredentialsResponse{}, nil
	}
	return &auth.CredentialsResponse{Username: a.username, Secret: a.password}, nil
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
// code or a vertex beside its line.
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
