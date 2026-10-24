package providertest

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// networkAsked is how many instances RunNetwork asks a place to hold: a gang of two.
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

// RunNetwork is the check of the Networker capability, for an adapter that implements it: Within
// is a level of nearness; the count for the template is within what was asked about and a size
// no machine has counts nothing; a place for two instances is chosen; an instance created in it
// is listed with an address, and the other reaches it at that address on the rendezvous port. It
// rents two instances at most, and skips when no place holds two right now. The template's image
// must also provide an nc that listens with nc -l -p.
func RunNetwork(t *testing.T, p provider.Provider, opts Options) {
	opts.validate(t)
	networker, ok := p.(provider.Networker)
	if !ok {
		t.Fatalf("%T does not implement provider.Networker", p)
	}
	if within := clusterapi.NodesLevel(networker.Within()); !slices.Contains(clusterapi.NodesLevels, within) {
		t.Fatalf("Within = %q, none of %v", within, clusterapi.NodesLevels)
	}
	paced := &paced{Provider: p}
	ctx := guard(t, paced)
	r := newRun(t, ctx, paced, opts)

	count := networked(t, ctx, networker, opts.Template)
	if none := networked(t, ctx, networker, noMachine(opts.Template)); none != 0 {
		t.Fatalf("Networked = %d for a size no machine has", none)
	}
	if count < networkAsked {
		t.Skipf("no place holds %d instances of the template right now", networkAsked)
	}

	port := strconv.Itoa(jobenv.DistributedMasterPort)
	listener := r.spec("listener", listenScript)
	listener.Env = map[string]string{"HL_CONFORMANCE_INTERFACE": networker.Interface(), "HL_CONFORMANCE_PORT": port}
	dialer := r.spec("dialer", dialScript)
	place, err := networker.Place(ctx, []provider.Spec{listener, dialer})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	t.Logf("place %s", place)
	listener.Place, dialer.Place = place, place

	listenerID, asked := r.create(t, ctx, listener)
	lines := r.awaitLine(t, ctx, listenerID, "conformance-listening", opts.StartTimeout)
	if !slices.Contains(lines, "conformance-interface-found") {
		t.Errorf("the instance has no interface %q: %q", networker.Interface(), lines)
	}
	first := r.awaitAddress(t, ctx, listenerID, opts.SettleTimeout)
	t.Logf("address %s known %s after Create", first.PrivateAddress, since(asked))

	dialer.Env = map[string]string{"HL_CONFORMANCE_PEER": first.PrivateAddress, "HL_CONFORMANCE_PORT": port}
	dialerID, asked := r.create(t, ctx, dialer)
	r.awaitLine(t, ctx, dialerID, "conformance-reached", opts.StartTimeout+opts.SettleTimeout)
	t.Logf("reached the other instance's address %s after Create", since(asked))
	r.awaitAddress(t, ctx, dialerID, opts.SettleTimeout)
}

func networked(t *testing.T, ctx context.Context, networker provider.Networker, spec provider.Spec) int {
	t.Helper()
	supply, err := networker.Networked(ctx, spec, networkAsked)
	if err != nil {
		t.Fatalf("Networked: %v", err)
	}
	if supply.Count < 0 || supply.Count > networkAsked {
		t.Fatalf("Networked = %d, asked about %d", supply.Count, networkAsked)
	}
	if (supply.Count == 0) != (supply.Why != "") {
		t.Fatalf("Networked = %+v: a count of nothing says why, and only such a count", supply)
	}
	return supply.Count
}

// awaitAddress waits until the instance is listed with the address the other members of its
// place reach it at.
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
