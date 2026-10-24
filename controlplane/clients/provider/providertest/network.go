package providertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// networkAsked is how many instances RunNetwork asks a network to hold: a gang of two.
const networkAsked = 2

const (
	listenScript = `if [ -d "/sys/class/net/$HL_CONFORMANCE_INTERFACE" ]; then echo conformance-interface-found; else echo conformance-interface-missing; fi
echo conformance-listening
while :; do echo conformance-peer | nc -l -p "$HL_CONFORMANCE_PORT"; done`

	dialScript = `n=0
until sleep 5 | nc "$HL_CONFORMANCE_PEER" "$HL_CONFORMANCE_PORT" | grep -q conformance-peer; do
	n=$((n+1))
	if [ $n -ge 60 ]; then echo conformance-unreached; exit 1; fi
	sleep 5
done
echo conformance-reached
while :; do sleep 1 & wait $!; done`
)

// RunNetwork is the check of the Networker capability, for an adapter that implements it: the
// count for the template is within what was asked about and a size no machine has counts
// nothing; a network for two instances on hosts of their own is made, and made again without
// error; an instance created on it is listed with an address, at which the other, on another host,
// reaches it on the rendezvous port; and once both are gone the network is deleted, repeatably.
// It rents two instances at most, and skips when no network holds two right now. The template's
// image must also provide an nc that listens with nc -l -p.
//
// Networks are swept by name as instances are (guard), at the start and the end of the check;
// a run killed outright leaves its network to the next one.
func RunNetwork(t *testing.T, p provider.Provider, opts Options) {
	opts.validate(t)
	networker, ok := p.(provider.Networker)
	if !ok {
		t.Fatalf("%T does not implement provider.Networker", p)
	}
	paced := &paced{Provider: p}
	ctx := guard(t, paced)
	if err := sweepNetworks(ctx, networker); err != nil {
		t.Fatalf("networks left by an earlier run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cleanupLimit)
		defer cancel()
		if err := sweepNetworks(ctx, networker); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	r := newRun(t, ctx, paced, opts)

	count := networked(t, ctx, networker, opts.Template)
	if count < 0 || count > networkAsked {
		t.Fatalf("Networked = %d, asked about %d", count, networkAsked)
	}
	noMachine := opts.Template
	noMachine.CPUMillis = 1 << 40
	if none := networked(t, ctx, networker, noMachine); none != 0 {
		t.Fatalf("Networked = %d for a size no machine has", none)
	}
	if count < networkAsked {
		t.Skipf("no private network holds %d instances of the template on hosts of their own right now", networkAsked)
	}

	name := r.prefix + "net"
	port := strconv.Itoa(jobenv.DistributedMasterPort)
	listener := r.spec("listener", listenScript)
	listener.Network = name
	listener.Env = map[string]string{"HL_CONFORMANCE_INTERFACE": networker.Interface(), "HL_CONFORMANCE_PORT": port}
	dialer := r.spec("dialer", dialScript)
	dialer.Network = name
	for range 2 {
		if err := networker.CreateNetwork(ctx, name, []provider.Spec{listener, dialer}, true); err != nil {
			t.Fatalf("CreateNetwork(%s): %v", name, err)
		}
	}
	if names, err := networker.Networks(ctx, r.prefix); err != nil || !slices.Equal(names, []string{name}) {
		t.Fatalf("Networks(%q) = %v, %v, want [%s]", r.prefix, names, err, name)
	}

	listenerID, asked := r.create(t, ctx, listener)
	lines := r.awaitLine(t, ctx, listenerID, "conformance-listening", opts.StartTimeout)
	if !slices.Contains(lines, "conformance-interface-found") {
		t.Errorf("the instance has no interface %q: %q", networker.Interface(), lines)
	}
	first := r.awaitAddress(t, ctx, listenerID, opts.SettleTimeout)
	t.Logf("address %s known %s after Create", first.PrivateAddress, since(asked))

	dialer.Env = map[string]string{"HL_CONFORMANCE_PEER": first.PrivateAddress, "HL_CONFORMANCE_PORT": port}
	if first.Host != "" {
		dialer.AvoidHosts = []string{first.Host}
	}
	dialerID, asked := r.create(t, ctx, dialer)
	r.awaitLine(t, ctx, dialerID, "conformance-reached", opts.StartTimeout+opts.SettleTimeout)
	t.Logf("reached the other instance's address %s after Create", since(asked))
	second := r.awaitAddress(t, ctx, dialerID, opts.SettleTimeout)
	if first.Host != "" && second.Host == first.Host {
		t.Errorf("both instances of a spread network are on host %s", first.Host)
	}

	for _, id := range []string{listenerID, dialerID} {
		if err := paced.Destroy(ctx, id); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
	}
	for _, id := range []string{listenerID, dialerID} {
		r.awaitGone(t, ctx, id, opts.SettleTimeout)
	}
	for range 2 {
		if err := networker.DeleteNetwork(ctx, name); err != nil {
			t.Fatalf("DeleteNetwork(%s): %v", name, err)
		}
	}
	deleted := time.Now()
	for {
		names, err := networker.Networks(ctx, r.prefix)
		if err != nil {
			t.Fatalf("Networks(%q): %v", r.prefix, err)
		}
		if len(names) == 0 {
			break
		}
		if time.Since(deleted) > opts.SettleTimeout {
			t.Fatalf("network %s is still listed %s after DeleteNetwork", name, opts.SettleTimeout)
		}
		pause(t, ctx)
	}
}

func networked(t *testing.T, ctx context.Context, networker provider.Networker, spec provider.Spec) int {
	t.Helper()
	supply, err := networker.Networked(ctx, spec, networkAsked, true)
	if err != nil {
		t.Fatalf("Networked: %v", err)
	}
	if (supply.Count == 0) != (supply.Why != "") {
		t.Fatalf("Networked = %+v: a count of nothing says why, and only such a count", supply)
	}
	return supply.Count
}

// awaitAddress waits until the instance is listed with the address the other members of its
// network reach it at.
func (r *run) awaitAddress(t *testing.T, ctx context.Context, id string, limit time.Duration) provider.Instance {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		i, listed := r.find(t, ctx, id)
		if !listed {
			t.Fatalf("instance %s left List while waiting for its private address", id)
		}
		if i.PrivateAddress != "" {
			return i
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s has no private address %s after it ran", id, limit)
		}
		pause(t, ctx)
	}
}

// sweepNetworks deletes every network of the suite on the account. The instances on them are
// destroyed first, by guard and by each run's cleanup.
func sweepNetworks(ctx context.Context, networker provider.Networker) error {
	names, err := networker.Networks(ctx, suitePrefix)
	if err != nil {
		return fmt.Errorf("list networks %q: %w", suitePrefix, err)
	}
	var failed []error
	for _, name := range names {
		if err := networker.DeleteNetwork(ctx, name); err != nil {
			failed = append(failed, fmt.Errorf("network %s is still there: %w", name, err))
		}
	}
	return errors.Join(failed...)
}
