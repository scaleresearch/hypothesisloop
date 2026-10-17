package podexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const (
	// jobNetwork is the engine network every job's container runs on, and jobBridge the host
	// interface of it the firewall rules name.
	jobNetwork = "hypothesisloop-jobs"
	jobBridge  = "hl-jobs"
	// firewallChain holds the rules, jumped to first from FORWARD.
	firewallChain = "HYPOTHESISLOOP-JOBS"
	// bridgeNetfilter makes traffic between two containers of one bridge pass FORWARD.
	bridgeNetfilter = "/proc/sys/net/bridge/bridge-nf-call-iptables"
)

// firewallRules keep a job from every other job's container and from the link-local range,
// where clouds serve instance metadata and a machine's credentials.
var firewallRules = [][]string{
	{"-i", jobBridge, "-o", jobBridge, "-j", "DROP"},
	{"-i", jobBridge, "-d", "169.254.0.0/16", "-j", "DROP"},
}

// jobFirewall is the host's firewall as the agent keeps it for jobs. iptables runs the iptables
// command with args, and bridgeNetfilter is the path of the switch it turns on.
type jobFirewall struct {
	iptables        func(args ...string) ([]byte, error)
	bridgeNetfilter string
}

func hostFirewall() jobFirewall {
	return jobFirewall{
		iptables: func(args ...string) ([]byte, error) {
			return exec.Command("iptables", append([]string{"-w"}, args...)...).CombinedOutput()
		},
		bridgeNetfilter: bridgeNetfilter,
	}
}

// ensure puts the rules in place and the jump to them first in FORWARD, where an engine that
// restarted may have put its own jumps above it.
func (f jobFirewall) ensure() error {
	if err := f.ensureBridgeNetfilter(); err != nil {
		return err
	}
	if _, err := f.iptables("-S", firewallChain); err != nil {
		if out, err := f.iptables("-N", firewallChain); err != nil {
			return fmt.Errorf("create the iptables chain %s: %v: %s", firewallChain, err, out)
		}
	}
	for _, rule := range firewallRules {
		if _, err := f.iptables(append([]string{"-C", firewallChain}, rule...)...); err == nil {
			continue
		}
		if out, err := f.iptables(append([]string{"-A", firewallChain}, rule...)...); err != nil {
			return fmt.Errorf("add the rule %q to %s: %v: %s", strings.Join(rule, " "), firewallChain, err, out)
		}
	}
	out, err := f.iptables("-S", "FORWARD")
	if err != nil {
		return fmt.Errorf("read the FORWARD chain: %v: %s", err, out)
	}
	jump := "-A FORWARD -j " + firewallChain
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 1 && lines[1] == jump && !slices.Contains(lines[2:], jump) {
		return nil
	}
	for {
		if _, err := f.iptables("-D", "FORWARD", "-j", firewallChain); err != nil {
			break
		}
	}
	if out, err := f.iptables("-I", "FORWARD", "1", "-j", firewallChain); err != nil {
		return fmt.Errorf("jump to %s first from FORWARD: %v: %s", firewallChain, err, out)
	}
	return nil
}

func (f jobFirewall) ensureBridgeNetfilter() error {
	if _, err := os.Stat(f.bridgeNetfilter); errors.Is(err, os.ErrNotExist) {
		if out, err := exec.Command("modprobe", "br_netfilter").CombinedOutput(); err != nil {
			return fmt.Errorf("load br_netfilter, which passes traffic between two jobs' containers through the firewall: %v: %s", err, out)
		}
	}
	if err := os.WriteFile(f.bridgeNetfilter, []byte("1"), 0o644); err != nil {
		return fmt.Errorf("pass traffic between two jobs' containers through the firewall: %w", err)
	}
	return nil
}

// requireHostFirewall refuses an agent whose firewall rules would not reach its jobs' traffic:
// they are the host's, so the engine must be this host's own and rootful, and setting them needs
// root.
func requireHostFirewall(info client.SystemInfoResult) error {
	why := "jobs are kept from each other and from 169.254.0.0/16 by the host's firewall, which the agent sets"
	if host := os.Getenv("DOCKER_HOST"); host != "" && !strings.HasPrefix(host, "unix://") {
		return fmt.Errorf("podexec: DOCKER_HOST %s is not an engine of this host, and %s", host, why)
	}
	if slices.Contains(info.Info.SecurityOptions, "name=rootless") {
		return fmt.Errorf("podexec: the engine is rootless, so its containers' traffic does not pass the host's firewall, and %s", why)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("podexec: the agent is not root, and %s", why)
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		return fmt.Errorf("podexec: %s with iptables, which is not installed: %w", why, err)
	}
	return nil
}

// networkEngine is the part of the Engine API that keeps the jobs' network.
type networkEngine interface {
	NetworkInspect(ctx context.Context, networkID string, options client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
}

// ensureJobNetwork creates the jobs' network when the engine has none, its bridge named for the
// firewall rules.
func ensureJobNetwork(ctx context.Context, engine networkEngine) error {
	_, err := engine.NetworkInspect(ctx, jobNetwork, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("podexec: inspect the network %s: %w", jobNetwork, err)
	}
	_, err = engine.NetworkCreate(ctx, jobNetwork, client.NetworkCreateOptions{
		Driver: "bridge",
		Options: map[string]string{
			"com.docker.network.bridge.name":       jobBridge,
			"com.docker.network.bridge.enable_icc": "false",
		},
		Labels: map[string]string{LabelManagedBy: ManagedByValue},
	})
	if err != nil && !errdefs.IsConflict(err) {
		return fmt.Errorf("podexec: create the network %s: %w", jobNetwork, err)
	}
	return nil
}
