package podexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

// fakeIptables is the filter table as iptables reads and changes it.
type fakeIptables struct {
	chains  map[string][]string
	changes int
}

func (f *fakeIptables) run(args ...string) ([]byte, error) {
	chain, rest := args[1], args[2:]
	rule := strings.Join(rest, " ")
	rules, exists := f.chains[chain]
	switch args[0] {
	case "-S":
		if !exists {
			return nil, errors.New("no chain")
		}
		out := "-N " + chain
		if chain == "FORWARD" {
			out = "-P FORWARD ACCEPT"
		}
		for _, r := range rules {
			out += "\n-A " + chain + " " + r
		}
		return []byte(out + "\n"), nil
	case "-N":
		f.chains[chain] = nil
	case "-C":
		if !slices.Contains(rules, rule) {
			return nil, errors.New("no rule")
		}
		return nil, nil
	case "-A":
		f.chains[chain] = append(rules, rule)
	case "-D":
		i := slices.Index(rules, rule)
		if i < 0 {
			return nil, errors.New("no rule")
		}
		f.chains[chain] = slices.Delete(rules, i, i+1)
	case "-I":
		f.chains[chain] = slices.Insert(rules, 0, strings.Join(rest[1:], " "))
	}
	f.changes++
	return nil, nil
}

// A job runs code of any org. Its container must reach no other job's and not the link-local
// range, where a cloud serves the machine's credentials, whatever an engine that restarted put
// in front of the rules.
func TestTheFirewallKeepsJobsApartAndFromTheLinkLocalRange(t *testing.T) {
	table := &fakeIptables{chains: map[string][]string{"FORWARD": {"-j DOCKER-USER", "-j DOCKER-FORWARD"}}}
	netfilter := filepath.Join(t.TempDir(), "bridge-nf-call-iptables")
	if err := os.WriteFile(netfilter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	firewall := jobFirewall{iptables: table.run, bridgeNetfilter: netfilter}
	if err := firewall.ensure(); err != nil {
		t.Fatal(err)
	}
	want := []string{"-i hl-jobs -o hl-jobs -j DROP", "-i hl-jobs -d 169.254.0.0/16 -j DROP"}
	if got := table.chains[firewallChain]; !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", firewallChain, got, want)
	}
	if got := table.chains["FORWARD"]; !slices.Equal(got, []string{"-j " + firewallChain, "-j DOCKER-USER", "-j DOCKER-FORWARD"}) {
		t.Fatalf("FORWARD = %q, want the jump to %s first", got, firewallChain)
	}
	if raw, _ := os.ReadFile(netfilter); string(raw) != "1" {
		t.Fatalf("bridge-nf-call-iptables = %q: traffic between two containers of the bridge does not pass FORWARD", raw)
	}

	changes := table.changes
	if err := firewall.ensure(); err != nil || table.changes != changes {
		t.Fatalf("a second ensure made %d changes (%v), want none", table.changes-changes, err)
	}

	table.chains["FORWARD"] = slices.Insert(table.chains["FORWARD"], 0, "-j DOCKER-USER")
	if err := firewall.ensure(); err != nil {
		t.Fatal(err)
	}
	if got := table.chains["FORWARD"]; !slices.Equal(got, []string{"-j " + firewallChain, "-j DOCKER-USER", "-j DOCKER-USER", "-j DOCKER-FORWARD"}) {
		t.Fatalf("FORWARD = %q after the engine put its jump first, want the jump to %s first once", got, firewallChain)
	}
}

// The rules are the host's: an engine elsewhere or a rootless one runs containers whose traffic
// they never see, so such an agent starts no job.
func TestAnAgentWhoseFirewallMissesItsJobsStartsNone(t *testing.T) {
	if err := requireHostFirewall(client.SystemInfoResult{Info: system.Info{SecurityOptions: []string{"name=seccomp", "name=rootless"}}}); err == nil || !strings.Contains(err.Error(), "rootless") {
		t.Errorf("a rootless engine: %v", err)
	}
	t.Setenv("DOCKER_HOST", "tcp://engine.test:2376")
	if err := requireHostFirewall(client.SystemInfoResult{}); err == nil || !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Errorf("an engine of another host: %v", err)
	}
}

func TestEveryJobRunsOnTheNetworkWhoseBridgeTheFirewallNames(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	pass(t, t.Context(), e, fakeJob("job", "image", 0))
	eventually(t, "the job runs", func() bool { return f.state("job") == "running" })
	f.mu.Lock()
	defer f.mu.Unlock()
	if mode := f.containers[containerName("job")].host.NetworkMode; string(mode) != jobNetwork {
		t.Errorf("the job runs on network %q, want %s", mode, jobNetwork)
	}
	if bridge := f.networks[jobNetwork].Options["com.docker.network.bridge.name"]; bridge != jobBridge {
		t.Errorf("the jobs' network has bridge %q, want %s", bridge, jobBridge)
	}
}

func TestNoJobStartsWhileJobsCannotBeKeptApart(t *testing.T) {
	f := newFakeEngine()
	e := newFakeExecutor(f)
	e.isolate = func(context.Context) error { return errors.New("iptables: permission denied") }
	if err := e.CreateWorkload(t.Context(), fakeJob("job", "image", 0)); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("CreateWorkload = %v, want the firewall's error", err)
	}
	if state := f.state("job"); state != "" || f.count(f.pulls, "image") != 0 {
		t.Fatalf("the job has a %q container and %d pulls", state, f.count(f.pulls, "image"))
	}
}
