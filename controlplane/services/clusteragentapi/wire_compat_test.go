package clusteragentapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// newAnswer is the reconcile answer the handler now gives for c.
func newAnswer(t *testing.T, h *Handler, c wireCase) clusterapi.ReconcileAnswer {
	t.Helper()
	answer := clusterapi.ReconcileAnswer{Jobs: make([]*clusterapi.DesiredJob, len(c.exps)), CheckpointWindow: c.checkpointWindow, Builds: c.builds, RegistryPull: c.registryPull}
	for i, exp := range c.exps {
		job, err := h.desiredJob(exp)
		if err != nil {
			t.Fatal(err)
		}
		answer.Jobs[i] = job
	}
	return answer
}

// The testdata holds the reconcile answers of these desired states as the control plane encoded
// them before the answer had a type of its own (each job a domain.Experiment through
// ForRuntime). Runtimes built then and runtimes built now read every answer during a rollout,
// both ways, so the two encodings must say the same: every key the answer now carries is in the
// old one with the same value, and every key it no longer carries held a zero value, which is
// what a runtime that decodes it gets for an absent key.
func TestTheReconcileAnswerIsTheOneRuntimesAlreadyRead(t *testing.T) {
	h := wireHandler(t)
	for name, c := range wireCases() {
		t.Run(name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", "reconcile_answer_"+name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			answer := newAnswer(t, h, c)
			wire, err := json.Marshal(answer)
			if err != nil {
				t.Fatal(err)
			}
			var old, now map[string]any
			if err := json.Unmarshal(golden, &old); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(wire, &now); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(keys(old), keys(now)) {
				t.Fatalf("answer keys = %v, were %v", keys(now), keys(old))
			}
			for key := range old {
				if key == "experiments" {
					continue
				}
				if !reflect.DeepEqual(old[key], now[key]) {
					t.Errorf("%s = %v, was %v", key, now[key], old[key])
				}
			}
			oldJobs, nowJobs := old["experiments"].([]any), now["experiments"].([]any)
			if len(oldJobs) != len(nowJobs) {
				t.Fatalf("%d jobs, were %d", len(nowJobs), len(oldJobs))
			}
			for i := range oldJobs {
				oldJob, nowJob := oldJobs[i].(map[string]any), nowJobs[i].(map[string]any)
				for key, value := range nowJob {
					if was, ok := oldJob[key]; !ok || !reflect.DeepEqual(was, value) {
						t.Errorf("job %d: %s = %v, was %v (present %v)", i, key, value, was, ok)
					}
				}
				for key, was := range oldJob {
					if _, ok := nowJob[key]; !ok && !zeroJSON(was) {
						t.Errorf("job %d: %s is no longer sent, and was %v", i, key, was)
					}
				}
			}

			// A runtime built now reads the old answer as the new one.
			var read clusterapi.ReconcileAnswer
			if err := json.Unmarshal(golden, &read); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(read, answer) {
				t.Errorf("the old answer reads as %+v, want %+v", read, answer)
			}
		})
	}
}

// A field added to the job spec reaches every runtime: the spec a runtime is handed encodes as
// the platform's own does, whatever is set on it.
func TestTheDesiredJobSpecEncodesAsThePlatformsJobSpec(t *testing.T) {
	var spec domain.JobSpec
	fill(t, "JobSpec", reflect.ValueOf(&spec).Elem())
	want, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(jobSpec(spec, spec.SecretEnv))
	if err != nil {
		t.Fatal(err)
	}
	var wantFields, gotFields map[string]any
	if err := json.Unmarshal(want, &wantFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &gotFields); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wantFields, gotFields) {
		t.Fatalf("the runtime's spec = %s\nthe platform's = %s", got, want)
	}
}

// A job's secret_env reaches its runtime on the spec, the resolved one included, and is written
// onto no job the platform holds.
func TestSecretEnvReachesTheRuntimeOnBothSpecsOnly(t *testing.T) {
	exp := &domain.Experiment{
		ID: "job-1", Job: domain.JobSpec{Image: "img"}, ResolvedJob: &domain.JobSpec{Image: "img"},
		SecretEnv: map[string]string{"HF_TOKEN": "the-secret-value"},
	}
	job, err := wireHandler(t).desiredJob(exp)
	if err != nil {
		t.Fatal(err)
	}
	if job.Job.SecretEnv["HF_TOKEN"] != "the-secret-value" || job.EffectiveJob().SecretEnv["HF_TOKEN"] != "the-secret-value" {
		t.Fatalf("desired job = %+v, want secret_env on job and resolved_job", job)
	}
	if exp.Job.SecretEnv != nil || exp.ResolvedJob.SecretEnv != nil {
		t.Fatalf("the secret was written onto the job it was read from: %+v", exp)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// zeroJSON is whether v decodes to a zero value: an absent key decodes the same.
func zeroJSON(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == "" || v == time.Time{}.Format(time.RFC3339)
	case float64:
		return v == 0
	case bool:
		return !v
	case map[string]any:
		for _, field := range v {
			if !zeroJSON(field) {
				return false
			}
		}
		return true
	}
	return false
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
		fill(t, name, v.Index(0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(t, name, key)
		fill(t, name, value)
		v.SetMapIndex(key, value)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(t, v.Type().Field(i).Name, v.Field(i))
			}
		}
	default:
		t.Fatalf("field %s has kind %s, which this test does not know how to set", name, v.Kind())
	}
}
