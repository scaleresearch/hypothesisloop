package clusteragentapi

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// The reconcile answer hands each job the key stored with it and asks the store nothing, so an
// object-store outage can never blank a cluster's desired state.
func TestAttachDataAccessHandsEachJobItsOwnStoredKey(t *testing.T) {
	// An unreachable endpoint: any store call would fail.
	store, err := objectstore.New("http://127.0.0.1:1", "us-east-1", "hl-data", "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{dataStore: store}
	a := &domain.Experiment{ID: "exp-1", AgentID: "agent-a", PlatformExperimentID: "pe-1",
		DataKey: domain.DataKey{AccessKeyID: "AK1", SecretAccessKey: "SK1"}}
	b := &domain.Experiment{ID: "exp-2", AgentID: "agent-b", PlatformExperimentID: "pe-1",
		DataKey: domain.DataKey{AccessKeyID: "AK2", SecretAccessKey: "SK2"}}

	h.attachDataAccess([]*domain.Experiment{a, b})

	want := domain.DataAccess{
		URI: "s3://hl-data/pe-1/agent-a/exp-1/", Shared: "s3://hl-data/pe-1/",
		Endpoint: "http://127.0.0.1:1", Region: "us-east-1",
		AccessKeyID: "AK1", SecretAccessKey: "SK1",
	}
	if *a.Data != want {
		t.Fatalf("data access = %+v, want %+v", *a.Data, want)
	}
	if b.Data.AccessKeyID != "AK2" || b.Data.SecretAccessKey != "SK2" {
		t.Fatalf("second job got key %q, want its own AK2", b.Data.AccessKeyID)
	}
}
