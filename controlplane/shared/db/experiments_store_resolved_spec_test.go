package db

import (
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// fakeExperimentRow feeds scanExperiment's fixed positional Scan call with test-controlled column
// values, without a live Postgres connection — this package has no DB integration harness, so
// this is the persistence-shape test available offline: it proves the exact wire contract
// ClaimSubmitted/scanExperiment agree on (resolved_job_spec is a column independent of job_spec).
type fakeExperimentRow struct {
	jobSpec, resolvedJobSpec []byte
}

func (r fakeExperimentRow) Scan(dest ...any) error {
	now := time.Now()
	values := []any{
		"exp-1", (*string)(nil), "agent-1", "pe-1", "", "cluster-a",
		"repo@" + fortyHexChars(), "cfg", "s3://data", r.jobSpec, r.resolvedJobSpec,
		"hyp-1", "hypothesis text", "objective", "theory",
		"example.com/product=test-accelerator", 2,
		1.0, 0.0,
		0.0, 0.0, "guaranteed", "SUBMITTED",
		&now, &now, (*string)(nil), (*string)(nil),
		(*time.Time)(nil), 0, 0, []byte("[]"),
		now, now,
	}
	if len(values) != len(dest) {
		panic("fakeExperimentRow: column count drifted from scanExperiment's Scan call")
	}
	for i, v := range values {
		assignScanDest(dest[i], v)
	}
	return nil
}

func fortyHexChars() string {
	return "0123456789abcdef0123456789abcdef01234567"[:40]
}

// assignScanDest copies v into dest, a pointer of the matching concrete type — a tiny reflection
// stand-in for what pgx's real Scan does, sufficient for the fixed, known column shape here.
func assignScanDest(dest, v any) {
	switch d := dest.(type) {
	case *string:
		*d = v.(string)
	case **string:
		*d = v.(*string)
	case *int:
		*d = v.(int)
	case *float64:
		*d = v.(float64)
	case *[]byte:
		*d = v.([]byte)
	case *time.Time:
		*d = v.(time.Time)
	case **time.Time:
		*d = v.(*time.Time)
	default:
		panic("assignScanDest: unhandled dest type")
	}
}

// The whole point of resolved_job_spec: an admitted experiment's literal resolved values are
// durably readable independent of any in-memory tick state, and the ORIGINAL job_spec — what the
// agent submitted — still round-trips verbatim, "max" and all. GET must never show a rewritten
// spec (see domain.Experiment.ResolvedJob's doc comment).
func TestScanExperimentKeepsOriginalJobSpecAndResolvedJobSpecIndependent(t *testing.T) {
	original := `{"cpu":"max","memory":"1Gi","storage":"max","accelerator_count":2,"accelerator_type":"example.com/product=test-accelerator","max_retries":0}`
	resolved := `{"cpu":"2","memory":"1Gi","storage":"200","accelerator_count":2,"accelerator_type":"example.com/product=test-accelerator","max_retries":0}`

	exp, err := scanExperiment(fakeExperimentRow{jobSpec: []byte(original), resolvedJobSpec: []byte(resolved)})
	if err != nil {
		t.Fatalf("scanExperiment: %v", err)
	}

	if exp.Job.CPU != domain.MaxResourceSentinel || exp.Job.Storage != domain.MaxResourceSentinel {
		t.Fatalf("exp.Job (original submitted spec) = %+v, want cpu/storage still \"max\" — GET must show exactly what was submitted", exp.Job)
	}
	if exp.ResolvedJob == nil {
		t.Fatal("exp.ResolvedJob is nil, want the durably persisted resolution")
	}
	if exp.ResolvedJob.CPU != "2" || exp.ResolvedJob.Storage != "200" {
		t.Fatalf("exp.ResolvedJob = %+v, want literal cpu=2 storage=200", exp.ResolvedJob)
	}
	// Independence: mutating one must never be visible through the other (they are not aliased
	// views of the same underlying data).
	exp.ResolvedJob.CPU = "999"
	if exp.Job.CPU == "999" {
		t.Fatal("exp.Job and exp.ResolvedJob alias the same memory — a downstream mutation of one leaked into the other")
	}
}

// A QUEUED experiment that has never been admitted has no resolution to report yet: a NULL
// resolved_job_spec column must leave ResolvedJob nil, not an empty-but-non-nil JobSpec (which
// domain.Experiment.EffectiveJob would then wrongly prefer over the real submitted Job).
func TestScanExperimentLeavesResolvedJobNilWhenColumnIsNull(t *testing.T) {
	exp, err := scanExperiment(fakeExperimentRow{
		jobSpec:         []byte(`{"cpu":"max","memory":"1Gi","storage":"1Gi","accelerator_count":2,"accelerator_type":"example.com/product=test-accelerator","max_retries":0}`),
		resolvedJobSpec: nil,
	})
	if err != nil {
		t.Fatalf("scanExperiment: %v", err)
	}
	if exp.ResolvedJob != nil {
		t.Fatalf("exp.ResolvedJob = %+v, want nil for a never-admitted experiment", exp.ResolvedJob)
	}
	if exp.EffectiveJob().CPU != domain.MaxResourceSentinel {
		t.Fatalf("EffectiveJob().CPU = %q, want \"max\" (falls back to Job when ResolvedJob is nil)", exp.EffectiveJob().CPU)
	}
}
