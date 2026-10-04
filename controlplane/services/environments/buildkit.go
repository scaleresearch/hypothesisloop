package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/solver/pb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// buildPlatform is the platform every environment is built for: the job and agent images are
// amd64.
const buildPlatform = "linux/amd64"

func (b *BuildKit) Build(ctx context.Context, dockerfile, base, image string) error {
	username, password, err := b.Credential(ctx)
	if err != nil {
		return fmt.Errorf("environments: registry credential: %w", err)
	}
	u, _ := url.Parse(b.Addr)
	c, err := client.New(ctx, b.Addr, client.WithServerConfig(u.Hostname(), b.CAFile), client.WithCredentials(b.CertFile, b.KeyFile))
	if err != nil {
		return fmt.Errorf("environments: connect to the builder: %w", err)
	}
	defer c.Close() //nolint:errcheck

	opt := client.SolveOpt{
		Exports: []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{"name": image, "push": "true"}}},
		Session: []session.Attachable{&registryAuth{host: b.RegistryHost, username: username, password: password}},
	}
	build := func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		platform := platforms.MustParse(buildPlatform)
		_, _, config, err := gw.ResolveImageConfig(ctx, base, sourceresolver.Opt{ImageOpt: &sourceresolver.ResolveImageOpt{Platform: &platform}})
		if err != nil {
			return nil, err
		}
		var baseImage struct{ Config struct{ User string } }
		if err := json.Unmarshal(config, &baseImage); err != nil {
			return nil, fmt.Errorf("the configuration of %s: %w", base, err)
		}
		inputs, err := frontendInputs(ctx, dockerfile)
		if err != nil {
			return nil, err
		}
		return gw.Solve(ctx, gateway.SolveRequest{
			Frontend: "dockerfile.v0",
			FrontendOpt: map[string]string{
				"build-arg:BASE":           base,
				"build-arg:" + baseUserArg: baseImage.Config.User,
				"platform":                 buildPlatform,
			},
			FrontendInputs: inputs,
		})
	}

	var log strings.Builder
	statuses := make(chan *client.SolveStatus)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		_, err := c.Build(groupCtx, opt, "", build, statuses)
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
			if log.Len() > 4*maxLogBytes {
				tail := LastBytes(log.String(), maxLogBytes)
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
	if builderFault(err) || ctx.Err() != nil {
		return fmt.Errorf("environments: build: %w", err)
	}
	return &BuildFailed{Log: log.String() + "\n" + err.Error()}
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

// frontendInputs is the Dockerfile and the build context the frontend reads it from: the same
// state, holding the file alone. The daemon refuses an input with no operation, which an empty
// llb.Scratch() is.
func frontendInputs(ctx context.Context, dockerfile string) (map[string]*pb.Definition, error) {
	file, err := llb.Scratch().File(llb.Mkfile("Dockerfile", 0o644, []byte(dockerfile))).Marshal(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]*pb.Definition{"dockerfile": file.ToPB(), "context": file.ToPB()}, nil
}

const maxLogBytes = 64 << 10

// builderFault tells an error of reaching the builder from a build the file failed.
func builderFault(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded, codes.Unauthenticated, codes.ResourceExhausted:
		return true
	}
	return false
}
