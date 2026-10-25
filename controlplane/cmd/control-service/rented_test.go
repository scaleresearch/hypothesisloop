package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	hypothesisloopcfg "github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// fakeRegistry holds cluster names by org as the store does: a name registered again for its own org
// changes nothing, and one another org holds is refused.
type fakeRegistry struct {
	owner map[string]string
	calls []string
}

func (r *fakeRegistry) RegisterCluster(_ context.Context, name, orgID string) (*domain.Cluster, error) {
	r.calls = append(r.calls, name)
	if owner, held := r.owner[name]; held && owner != orgID {
		return nil, db.ErrClusterNameTaken
	}
	r.owner[name] = orgID
	return &domain.Cluster{Name: name, OrgID: orgID}, nil
}

func TestEveryCloudComputeEntryIsRegisteredForThePlatformAtStartup(t *testing.T) {
	pcfg := &hypothesisloopcfg.ControlService{Settings: &hypothesisloopcfg.Settings{}}
	pcfg.CloudCompute = []hypothesisloopcfg.CloudComputeConfig{{ClusterName: "vast"}, {ClusterName: "runpod"}}
	r := &fakeRegistry{owner: map[string]string{"vast": domain.PlatformOrgID}}
	for range 2 {
		if err := registerCloudClusters(context.Background(), r, pcfg); err != nil {
			t.Fatal(err)
		}
	}
	if r.owner["vast"] != domain.PlatformOrgID || r.owner["runpod"] != domain.PlatformOrgID || !slices.Equal(r.calls, []string{"vast", "runpod", "vast", "runpod"}) {
		t.Fatalf("got owners %v after calls %v, want both the platform's", r.owner, r.calls)
	}

	r = &fakeRegistry{owner: map[string]string{"runpod": "org-other"}}
	err := registerCloudClusters(context.Background(), r, pcfg)
	if !errors.Is(err, db.ErrClusterNameTaken) || !strings.Contains(err.Error(), "cloud_compute runpod") {
		t.Fatalf("a name another org holds: got %v, want it refused, naming the entry", err)
	}
}
