package quota

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// GET /resource-catalog/capacity used to answer 200 with an empty cluster list when the handler
// was never wired to a metrics DB (metricsDBURL == ""), which reads exactly like "no cluster
// currently has this type free" — a job submitted against a real type then queues forever with no
// error (see the endpoint's own doc). That silently converts an operator misconfiguration into
// starvation nobody can diagnose from the API. Fail fast instead (important.md: "no fallbacks -
// one path or error, fail fast").
func TestLiveCapacityFailsFastWhenUnconfigured(t *testing.T) {
	h := NewPlatformExperimentsHandler(nil, nil)
	// Deliberately never call WithLiveCapacity.

	clusters, err := h.liveCapacity(context.Background())
	if err == nil {
		t.Fatalf("liveCapacity() with no metrics DB configured returned nil error and clusters=%v, want ErrLiveCapacityUnconfigured", clusters)
	}
	if !errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Fatalf("liveCapacity() error = %v, want ErrLiveCapacityUnconfigured", err)
	}
	if clusters != nil {
		t.Fatalf("liveCapacity() clusters = %v, want nil on error", clusters)
	}
}

// Once wired, an empty result must still be possible on its own merits (e.g. a metrics DB that is
// reachable but simply has no fresh reports) — this test just pins that WithLiveCapacity flips the
// handler out of the unconfigured error path; the live-read behavior itself is metricsdb's own.
func TestLiveCapacityConfiguredDoesNotFailFast(t *testing.T) {
	h := NewPlatformExperimentsHandler(nil, nil).WithLiveCapacity("postgres://example-not-dialed/db", time.Minute, nil)
	if h.metricsDBURL == "" {
		t.Fatalf("WithLiveCapacity did not set metricsDBURL")
	}
	// Not asserting on the outcome of an actual dial here — that's metricsdb's own concern and
	// would require a live DB. This only pins that liveCapacity no longer takes the "unconfigured"
	// branch once WithLiveCapacity has been called.
	_, err := h.liveCapacity(auth.WithPrincipal(context.Background(), auth.Principal{Kind: auth.KindStaff}))
	if errors.Is(err, ErrLiveCapacityUnconfigured) {
		t.Fatalf("liveCapacity() still reports unconfigured after WithLiveCapacity was called")
	}
}

// A job's cost is its run time times the rate of its accelerator type, so the capacity read an
// agent picks its type from carries that rate.
func TestCapacityCarriesEachTypesAccHRate(t *testing.T) {
	domain.SetAcceleratorRates(map[string]float64{"h100": 1.0, "gpu.product=L40": 0.25})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{"h100": 1.0}) })

	clusters := buildClusterCapacity(
		map[string]map[string]int64{"local": {"gpu.product=L40": 3}},
		map[string]map[string]int64{"local": {"gpu.product=L40": 8}})

	want := AcceleratorCapacity{AcceleratorType: "gpu.product=L40", Available: 3, Total: 8, AccHPerHour: 0.25}
	if len(clusters) != 1 || len(clusters[0].Accelerators) != 1 || clusters[0].Accelerators[0] != want {
		t.Fatalf("capacity = %+v, want one cluster with %+v", clusters, want)
	}
}

// A caller reads the capacity of the clusters its jobs can land on and of no other: knowing what
// another org runs on is knowing something of that org.
func TestCapacityShowsOnlyClustersThatTakeTheCallersJobs(t *testing.T) {
	registered := map[string]domain.Cluster{
		"of-a":   {Name: "of-a", OrgID: "org-a"},
		"of-b":   {Name: "of-b", OrgID: "org-b"},
		"shared": {Name: "shared", OrgID: "org-staff", Shared: true},
	}
	connected := []ClusterCapacity{{ClusterName: "of-a"}, {ClusterName: "of-b"}, {ClusterName: "phantom"}, {ClusterName: "shared"}}
	for name, tc := range map[string]struct {
		orgs []string
		want []string
	}{
		"a caller of one org":         {[]string{"org-a"}, []string{"of-a", "shared"}},
		"a person in two orgs":        {[]string{"org-a", "org-b"}, []string{"of-a", "of-b", "shared"}},
		"an org with no cluster":      {[]string{"org-c"}, []string{"shared"}},
		"a person who is in no org":   {nil, []string{"shared"}},
		"the org that shares its own": {[]string{"org-staff"}, []string{"shared"}},
	} {
		got := []string{}
		for _, c := range placeableBy(tc.orgs, registered, connected) {
			got = append(got, c.ClusterName)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got = %v, want = %v", name, got, tc.want)
		}
	}
}
