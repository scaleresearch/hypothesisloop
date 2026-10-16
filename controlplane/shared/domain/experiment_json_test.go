package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNoAnswerOfTheAPICarriesTheClustersAJobWasTriedOn(t *testing.T) {
	exp := &Experiment{ID: "job-1", TriedClusters: []TriedCluster{{ClusterID: "id-of-another-orgs-cluster", At: time.Unix(1700000000, 0)}}}
	wire, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "id-of-another-orgs-cluster") || strings.Contains(string(wire), "tried_clusters") {
		t.Fatalf("a job's JSON carries the ids of the clusters it was tried on: %s", wire)
	}
}

// A job's secret_env is on no JSON of the job: only the desired state of its runtime carries it.
func TestSecretEnvIsOnNoJSONOfTheJob(t *testing.T) {
	exp := &Experiment{
		ID: "job-1", Job: JobSpec{Image: "img", Env: map[string]string{"LR": "0.1"}},
		ResolvedJob: &JobSpec{Image: "img"},
		SecretEnv:   map[string]string{"HF_TOKEN": "the-secret-value"},
	}
	wire, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "the-secret-value") || strings.Contains(string(wire), "secret_env") {
		t.Fatalf("a job's JSON carries its secret_env: %s", wire)
	}
}
