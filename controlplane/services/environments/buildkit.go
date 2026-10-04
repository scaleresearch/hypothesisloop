package environments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// RegistryCredential is what the builder pushes to registryHost with, asked for at each build: a
// token lives about an hour.
type RegistryCredential func(ctx context.Context) (username, password string, err error)

// BuildKit builds on a BuildKit daemon reached at Addr over mutual TLS. The PEM files are read at
// each build: attaching the builder again issues new ones.
type BuildKit struct {
	Addr              string
	CAFile            string
	CertFile, KeyFile string
	RegistryHost      string
	Credential        RegistryCredential
}

var _ Builder = (*BuildKit)(nil)

func NewBuildKit(addr, caFile, certFile, keyFile, registryHost string, credential RegistryCredential) (*BuildKit, error) {
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "tcp" || u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("environments: the builder address %q is not tcp://host:port", addr)
	}
	if caFile == "" || certFile == "" || keyFile == "" || registryHost == "" || credential == nil {
		return nil, errors.New("environments: the builder's CA, certificate and key, the registry and its credential are required")
	}
	return &BuildKit{Addr: addr, CAFile: caFile, CertFile: certFile, KeyFile: keyFile, RegistryHost: registryHost, Credential: credential}, nil
}

// buildPlatform is the platform every environment is built for: the job images are amd64.
const buildPlatform = "linux/amd64"

// connect reaches the daemon, with the session that hands it the registry's credential.
func (b *BuildKit) connect(ctx context.Context) (*client.Client, []session.Attachable, error) {
	username, password, err := b.Credential(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("environments: registry credential: %w", err)
	}
	u, _ := url.Parse(b.Addr)
	c, err := client.New(ctx, b.Addr, client.WithServerConfig(u.Hostname(), b.CAFile), client.WithCredentials(b.CertFile, b.KeyFile))
	if err != nil {
		return nil, nil, fmt.Errorf("environments: connect to the builder: %w", err)
	}
	return c, []session.Attachable{&registryAuth{host: b.RegistryHost, username: username, password: password}}, nil
}

// Pull runs a command on image, which the daemon pulls and unpacks for, so its layers stay in the
// daemon's cache.
func (b *BuildKit) Pull(ctx context.Context, image string) error {
	c, attachables, err := b.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck
	def, err := llb.Image(image, llb.Platform(platforms.MustParse(buildPlatform))).Run(llb.Args([]string{"true"})).Root().Marshal(ctx)
	if err != nil {
		return err
	}
	if _, err := c.Solve(ctx, def, client.SolveOpt{Session: attachables}, nil); err != nil {
		return fmt.Errorf("environments: pull %s: %w", image, err)
	}
	return nil
}

func (b *BuildKit) Build(ctx context.Context, dockerfile, base, image string) error {
	c, attachables, err := b.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck
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
	err = group.Wait()
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
