package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A hypothesis has one implementation per agent and branch its jobs ran on, each at the commit of
// its newest job, and a new one starts from the commit of the newest completed job.
func TestImplementationsAndTheLastCompletedCommitAreReadOffTheJobs(t *testing.T) {
	store, first := admitDataKeyFixture(t)
	ctx := context.Background()
	other := "impl-agent-" + uuid.New().String()[:8]
	createTestAgent(t, store.ExperimentsStore.pool, other)
	hypothesis := "hyp-" + uuid.New().String()[:8]
	base := time.Now().UTC().Add(-time.Hour)
	ref := func(c string) string { return "repo@" + c + fortyHexChars()[1:] }
	for i, job := range []struct {
		agent, branch, commit string
		status                domain.ExperimentStatus
	}{
		{first.AgentID, "hl/pe/one/idea-abc123", "a", domain.StatusCompleted},
		{first.AgentID, "hl/pe/one/idea-abc123", "b", domain.StatusCompleted},
		{other, "hl/pe/two/idea-abc123", "c", domain.StatusCompleted},
		{first.AgentID, "hl/pe/one/idea-abc123", "d", domain.StatusFailed},
		{first.AgentID, "", "e", domain.StatusFailed},
	} {
		exp := *first
		exp.ID, exp.AgentID, exp.Actor.ID = first.ID+"-"+job.commit, job.agent, job.agent
		exp.HypothesisID, exp.Branch, exp.CodeRef, exp.Status = hypothesis, job.branch, ref(job.commit), job.status
		exp.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if err := createExperiment(ctx, store.ExperimentsStore.pool.pool, &exp); err != nil {
			t.Fatalf("create job %s: %v", job.commit, err)
		}
	}

	got, err := store.Implementations(ctx, hypothesis)
	want := []domain.Implementation{
		{AgentID: first.AgentID, Branch: "hl/pe/one/idea-abc123", CodeRef: ref("d")},
		{AgentID: other, Branch: "hl/pe/two/idea-abc123", CodeRef: ref("c")},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Implementations = %+v, %v, want %+v", got, err, want)
	}
	if none, err := store.Implementations(ctx, "hyp-untested"); err != nil || len(none) != 0 || none == nil {
		t.Errorf("Implementations of a hypothesis no job ran = %#v, %v, want an empty list", none, err)
	}
	if last, err := store.LastCompletedCodeRef(ctx, hypothesis); err != nil || last != ref("c") {
		t.Errorf("LastCompletedCodeRef = %q, %v, want %q", last, err, ref("c"))
	}
	if last, err := store.LastCompletedCodeRef(ctx, "hyp-untested"); err != nil || last != "" {
		t.Errorf("LastCompletedCodeRef of a hypothesis no job ran = %q, %v, want none", last, err)
	}

	if declared, err := store.BaselineCodeRef(ctx, first.PlatformExperimentID); err != nil || declared != "" {
		t.Errorf("BaselineCodeRef before a baseline is declared = %q, %v, want none", declared, err)
	}
	if _, err := store.ExperimentsStore.pool.pool.Exec(ctx, `UPDATE platform_experiments SET baseline_job_id = $2 WHERE id = $1`, first.PlatformExperimentID, first.ID+"-a"); err != nil {
		t.Fatal(err)
	}
	if declared, err := store.BaselineCodeRef(ctx, first.PlatformExperimentID); err != nil || declared != ref("a") {
		t.Errorf("BaselineCodeRef = %q, %v, want %q", declared, err, ref("a"))
	}
}
