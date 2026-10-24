package providertest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
)

func TestTheSweepDeletesTheSuitesNetworksAndNoOthers(t *testing.T) {
	ctx := context.Background()
	p := fake.New()
	for _, name := range []string{suitePrefix + "a-net", suitePrefix + "b-net", "hl-net-prod-e1-a0"} {
		if err := p.CreateNetwork(ctx, name, nil, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Create(ctx, provider.Spec{Name: suitePrefix + "b-listener", Architecture: "amd64", Network: suitePrefix + "b-net"}); err != nil {
		t.Fatal(err)
	}
	err := sweepNetworks(ctx, p)
	if err == nil || !strings.Contains(err.Error(), suitePrefix+"b-net") {
		t.Fatalf("a network with an instance on it was swept without a word: %v", err)
	}
	if left, _ := p.Networks(ctx, ""); !slices.Equal(left, []string{"hl-net-prod-e1-a0", suitePrefix + "b-net"}) {
		t.Fatalf("networks left = %v", left)
	}
}
