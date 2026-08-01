package podexec

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/moby/moby/client"
)

// candidateEngineSockets is tried in order at startup: podman's rootful and rootless
// Docker-compatible sockets first (podman is this runtime's documented recommendation — no
// daemon, plain process tree), then dockerd's own socket. DOCKER_HOST, if set, always wins.
func candidateEngineSockets() []struct{ name, host string } {
	return []struct{ name, host string }{
		{"podman", "unix:///run/podman/podman.sock"},
		{"podman", fmt.Sprintf("unix:///run/user/%d/podman/podman.sock", os.Getuid())},
		{"docker", client.DefaultDockerHost},
	}
}

// resolveEngineClient connects to whichever container engine is actually reachable, once at
// startup — every subsequent call in this package reuses the same *client.Client. Podman
// exposes a Docker-compatible API on its own socket, so one client implementation talks to
// either engine unmodified; only the socket path differs.
func resolveEngineClient(ctx context.Context) (*client.Client, string, error) {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, "", fmt.Errorf("podexec: connect to DOCKER_HOST %s: %w", host, err)
		}
		if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
			return nil, "", fmt.Errorf("podexec: ping DOCKER_HOST %s: %w", host, err)
		}
		return cli, "docker", nil
	}
	var errs []error
	for _, candidate := range candidateEngineSockets() {
		cli, err := client.NewClientWithOpts(client.WithHost(candidate.host), client.WithAPIVersionNegotiation())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("%s (%s): %w", candidate.name, candidate.host, err))
			continue
		}
		return cli, candidate.name, nil
	}
	return nil, "", fmt.Errorf("podexec: no reachable container engine (tried podman then docker sockets): %v", errs)
}

func localNodeName() (string, error) {
	name, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return name, nil
}

// containerName derives a deterministic, engine-legal container name from an experiment ID
// (names must match [a-zA-Z0-9][a-zA-Z0-9_.-]*) — the reconciliation identity, exactly like
// k8sexec.jobName.
func containerName(experimentID string) string {
	name := containerNamePrefix + sanitizeName(experimentID)
	if len(name) > 200 {
		h := sha256.Sum256([]byte(experimentID))
		suffix := fmt.Sprintf("-%x", h[:4])
		name = name[:200-len(suffix)] + suffix
	}
	return name
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func traceparentFromID(id string) string {
	h := sha256.Sum256([]byte(id))
	return fmt.Sprintf("00-%x-%x-01", h[:16], h[16:24])
}
