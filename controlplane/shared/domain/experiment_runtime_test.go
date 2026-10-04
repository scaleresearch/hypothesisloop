package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// filled returns an Experiment with every field set to something other than its zero value.
func filled(t *testing.T) *Experiment {
	t.Helper()
	exp := &Experiment{}
	v := reflect.ValueOf(exp).Elem()
	for i := 0; i < v.NumField(); i++ {
		fill(t, v.Type().Field(i).Name, v.Field(i))
	}
	return exp
}

func fill(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("set-" + name)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(t, name, v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(1700000000, 0)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(t, name, v.Field(i))
			}
		}
	default:
		t.Fatalf("field %s has kind %s, which this test does not know how to set", name, v.Kind())
	}
}

// A field added to Experiment reaches no runtime until it is named here and in ForRuntime.
func TestARuntimeIsHandedOnlyWhatItBuildsAWorkloadFrom(t *testing.T) {
	handed := map[string]bool{
		"ID": true, "AgentID": true, "ProjectID": true, "CodeRef": true, "ConfigHash": true, "DataRef": true,
		"Job": true, "ResolvedJob": true, "AcceleratorType": true, "AcceleratorCount": true,
		"EstimatedDurationHours": true, "CapacityTier": true, "AttemptCount": true, "SubmittedAt": true, "Data": true, "Token": true, "EndsAt": true, "EnvironmentKey": true,
	}
	full := filled(t)
	view := reflect.ValueOf(full.ForRuntime()).Elem()
	whole := reflect.ValueOf(full).Elem()
	for i := 0; i < view.NumField(); i++ {
		name := view.Type().Field(i).Name
		switch {
		case handed[name] && !reflect.DeepEqual(view.Field(i).Interface(), whole.Field(i).Interface()):
			t.Errorf("%s is handed to a runtime changed: got = %v, want = %v", name, view.Field(i).Interface(), whole.Field(i).Interface())
		case !handed[name] && !view.Field(i).IsZero():
			t.Errorf("%s is handed to a runtime, which has no use for it: %v", name, view.Field(i).Interface())
		}
	}

	wire, err := json.Marshal(full.ForRuntime())
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"set-OrgID", "set-PlatformExperimentID", "set-HypothesisID", "set-Hypothesis", "set-Theory", "set-ClusterName", "set-Summary", "set-NotAdmittedReason", "set-EvictionReason"} {
		if strings.Contains(string(wire), kept) {
			t.Errorf("the runtime's view carries %s on the wire: %s", kept, wire)
		}
	}
	var decoded Experiment
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("the view does not decode as the Experiment a runtime reads: %v", err)
	}
	if decoded.ID != full.ID || decoded.Token != full.Token || decoded.Data == nil || decoded.AttemptCount != full.AttemptCount || decoded.EffectiveJob().Image != full.ResolvedJob.Image ||
		decoded.SubmittedAt == nil || !decoded.SubmittedAt.Equal(*full.SubmittedAt) {
		t.Errorf("decoded view = %+v, want the id, token, data access, attempt, admission time and resolved job it was built with", decoded)
	}
}

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

// A job's secret_env is on no JSON of the job but the one a runtime is handed, where it sits on
// the spec the runtime builds the environment from, the resolved one included.
func TestSecretEnvIsOnTheWireOnlyForARuntime(t *testing.T) {
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

	wire, err = json.Marshal(exp.ForRuntime())
	if err != nil {
		t.Fatal(err)
	}
	var decoded Experiment
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Job.SecretEnv["HF_TOKEN"] != "the-secret-value" || decoded.EffectiveJob().SecretEnv["HF_TOKEN"] != "the-secret-value" {
		t.Fatalf("the runtime's view = %s, want secret_env on job and resolved_job", wire)
	}
	if exp.Job.SecretEnv != nil || exp.ResolvedJob.SecretEnv != nil {
		t.Fatalf("ForRuntime wrote the secret onto the job it was called on: %+v", exp)
	}
}
