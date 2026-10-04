package environments

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"
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

	dir, err := os.MkdirTemp("", "environment-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	empty := filepath.Join(dir, "context")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return err
	}
	contextFS, err := fsutil.NewFS(empty)
	if err != nil {
		return err
	}
	dockerfileFS, err := fsutil.NewFS(dir)
	if err != nil {
		return err
	}

	registry := b.RegistryHost
	auth := authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{
		AuthConfigProvider: func(_ context.Context, host string, _ []string, _ authprovider.ExpireCachedAuthCheck) (types.AuthConfig, error) {
			if host != registry {
				return types.AuthConfig{}, nil
			}
			return types.AuthConfig{ServerAddress: host, Username: username, Password: password}, nil
		},
	})
	opt := client.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename":       "Dockerfile",
			"build-arg:BASE": base,
			"platform":       buildPlatform,
		},
		LocalMounts: map[string]fsutil.FS{"context": contextFS, "dockerfile": dockerfileFS},
		Exports:     []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{"name": image, "push": "true"}}},
		Session:     []session.Attachable{auth},
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
