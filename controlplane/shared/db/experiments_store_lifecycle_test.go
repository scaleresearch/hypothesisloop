package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestAJobTheAdmissionLoopHasNotSeenIsAwaitingAdmission(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps := NewExperimentsStore(pool)
	ctx := context.Background()
	reason := func(id string) string {
		t.Helper()
		exp, err := exps.GetExperiment(ctx, id)
		if err != nil || exp == nil {
			t.Fatalf("GetExperiment(%s): got = %v, %v", id, exp, err)
		}
		return exp.NotAdmittedReason
	}

	seedExperimentForCapTest(t, pool, "awaiting-new-"+peID, agentID, peID, 1, domain.StatusQueued)
	if got := reason("awaiting-new-" + peID); got != domain.NotAdmittedAwaitingAdmission {
		t.Errorf("a new job: not_admitted_reason = %q, want %q", got, domain.NotAdmittedAwaitingAdmission)
	}

	seedExperimentForCapTest(t, pool, "awaiting-requeued-"+peID, agentID, peID, 1, domain.StatusRunning)
	requeued, err := exps.RequeuePreempted(ctx, "awaiting-requeued-"+peID, 1, 1)
	if err != nil || !requeued {
		t.Fatalf("RequeuePreempted: got = %v, %v, want true", requeued, err)
	}
	if got := reason("awaiting-requeued-" + peID); got != domain.NotAdmittedAwaitingAdmission {
		t.Errorf("a requeued job: not_admitted_reason = %q, want %q", got, domain.NotAdmittedAwaitingAdmission)
	}
}

func TestMarkUnderperformingIsTrueOncePerJob(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps := NewExperimentsStore(pool)
	id := "underperforming-" + peID
	seedExperimentForCapTest(t, pool, id, agentID, peID, 1, domain.StatusRunning)

	for i, want := range []bool{true, false} {
		got, err := exps.MarkUnderperforming(context.Background(), id)
		if err != nil || got != want {
			t.Errorf("call %d: MarkUnderperforming = %v, %v, want %v", i+1, got, err, want)
		}
	}
}

// A job read says when the job ended and carries the summary filed for it, so no reader has to
// infer the first from updated_at or search a hypothesis's findings for the second.
func TestAFinishedJobCarriesItsFinishTimeAndItsFiledSummary(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps, hyps := NewExperimentsStore(pool), NewHypothesesStore(pool)
	ctx := context.Background()
	id := "finished-" + peID
	seedExperimentForCapTest(t, pool, id, agentID, peID, 1, domain.StatusRunning)

	running, err := exps.GetExperiment(ctx, id)
	if err != nil || running.FinishedAt != nil {
		t.Fatalf("a running job: finished_at = %v, %v, want nil", running.FinishedAt, err)
	}
	if got, err := hyps.FindingSummary(ctx, id); err != nil || got != "" {
		t.Errorf("FindingSummary before one is filed: got = %q, %v, want empty", got, err)
	}

	if ok, err := exps.TransitionStatus(ctx, id, domain.StatusRunning, domain.StatusCompleted); err != nil || !ok {
		t.Fatalf("TransitionStatus: got = %v, %v, want true", ok, err)
	}
	if _, err := hyps.CreateHypothesisFinding(ctx, running.HypothesisID, peID, id, agentID, "it held"); err != nil {
		t.Fatalf("CreateHypothesisFinding: %v", err)
	}
	done, err := exps.GetExperiment(ctx, id)
	if err != nil || done.FinishedAt == nil || !done.FinishedAt.After(running.UpdatedAt) {
		t.Fatalf("a completed job: finished_at = %v, %v, want a time after %v", done.FinishedAt, err, running.UpdatedAt)
	}
	if err := exps.MarkQuotaSettled(ctx, id); err != nil {
		t.Fatalf("MarkQuotaSettled: %v", err)
	}
	if settled, err := exps.GetExperiment(ctx, id); err != nil || !settled.FinishedAt.Equal(*done.FinishedAt) {
		t.Errorf("after settlement: finished_at = %v, %v, want it unmoved at %v", settled.FinishedAt, err, done.FinishedAt)
	}
	if got, err := hyps.FindingSummary(ctx, id); err != nil || got != "it held" {
		t.Errorf("FindingSummary: got = %q, %v, want %q", got, err, "it held")
	}
}

func TestFindingsReadsCoverManyHypothesesInOneQueryEach(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps, hyps := NewExperimentsStore(pool), NewHypothesesStore(pool)
	ctx := context.Background()
	first, second, other := "many-first-"+peID, "many-second-"+peID, "many-other-"+peID
	for _, id := range []string{first, second, other} {
		seedExperimentForCapTest(t, pool, id, agentID, peID, 1, domain.StatusCompleted)
	}
	shared := "many-hypothesis-" + peID
	if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET hypothesis_id = $1 WHERE id = ANY($2)`, shared, []string{first, second}); err != nil {
		t.Fatalf("point two jobs at one hypothesis: %v", err)
	}
	for _, id := range []string{first, second} {
		if _, err := hyps.CreateHypothesisFinding(ctx, shared, peID, id, agentID, "summary of "+id); err != nil {
			t.Fatalf("CreateHypothesisFinding: %v", err)
		}
	}

	jobs, err := exps.ListExperimentsTesting(ctx, []string{shared, "no-such-hypothesis"})
	if err != nil || len(jobs) != 2 || jobs[0].ID != first || jobs[1].ID != second {
		t.Errorf("ListExperimentsTesting: got = %v, %v, want %s then %s", jobs, err, first, second)
	}
	newest, err := hyps.NewestFindings(ctx, []string{shared, "no-such-hypothesis"})
	if err != nil || len(newest) != 1 || newest[shared].ExperimentID != second || newest[shared].Summary != "summary of "+second {
		t.Errorf("NewestFindings: got = %v, %v, want the finding of %s only", newest, err, second)
	}
	summaries, err := hyps.FindingSummaries(ctx, []string{first, other})
	if err != nil || len(summaries) != 1 || summaries[first] != "summary of "+first {
		t.Errorf("FindingSummaries: got = %v, %v, want the summary of %s only", summaries, err, first)
	}
}

// $evidence is all a graph query sees of Postgres, so each entry has to count one platform
// experiment's rows only, keep rejected jobs out of jobs, and leave a hypothesis nobody touched
// without an entry.
func TestEvidenceCountsJobsByStatusFindingsAndCommentsOfOnePlatformExperiment(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	_, _, otherAgent, otherPE := setupCapTestPool(t, 4)
	hyps := NewHypothesesStore(pool)
	ctx := context.Background()
	busy, discussed, refused := "busy-"+peID, "discussed-"+peID, "refused-"+peID
	seed := func(id, agent, pe, hypothesisID string, status domain.ExperimentStatus) {
		t.Helper()
		seedExperimentForCapTest(t, pool, id, agent, pe, 1, status)
		if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET hypothesis_id = $1 WHERE id = $2`, hypothesisID, id); err != nil {
			t.Fatalf("point %s at %s: %v", id, hypothesisID, err)
		}
	}
	seed("done-1-"+peID, agentID, peID, busy, domain.StatusCompleted)
	seed("done-2-"+peID, agentID, peID, busy, domain.StatusCompleted)
	seed("running-"+peID, agentID, peID, busy, domain.StatusRunning)
	seed("rejected-"+peID, agentID, peID, busy, domain.StatusRejected)
	seed("only-rejected-"+peID, agentID, peID, refused, domain.StatusRejected)
	seed("elsewhere-"+peID, otherAgent, otherPE, busy, domain.StatusCompleted)
	finding, err := hyps.CreateHypothesisFinding(ctx, busy, peID, "done-1-"+peID, agentID, "it held")
	if err != nil {
		t.Fatalf("CreateHypothesisFinding: %v", err)
	}
	if _, err := hyps.CreateHypothesisFinding(ctx, busy, otherPE, "elsewhere-"+peID, otherAgent, "elsewhere"); err != nil {
		t.Fatalf("CreateHypothesisFinding: %v", err)
	}
	for _, c := range [][3]string{{busy, peID, agentID}, {discussed, peID, agentID}, {discussed, peID, agentID}, {busy, otherPE, otherAgent}} {
		if _, err := hyps.CreateHypothesisComment(ctx, c[0], c[1], domain.HypothesisSourceAgent, c[2], "", "note"); err != nil {
			t.Fatalf("CreateHypothesisComment: %v", err)
		}
	}

	got, err := hyps.Evidence(ctx, peID)
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	entry := func(jobs, completed, running, rejected, findings, comments int64) map[string]any {
		return map[string]any{"jobs": jobs, "submitted": int64(0), "queued": int64(0), "running": running,
			"completed": completed, "failed": int64(0), "evicted": int64(0), "rejected": rejected, "findings": findings, "comments": comments}
	}
	busyEntry := entry(3, 2, 1, 1, 1, 1)
	busyEntry["last_finding_at"] = got[busy].(map[string]any)["last_finding_at"]
	want := map[string]any{busy: busyEntry, discussed: entry(0, 0, 0, 0, 0, 2), refused: entry(0, 0, 0, 1, 0, 0)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Evidence: got = %v, want %v", got, want)
	}
	if at, ok := busyEntry["last_finding_at"].(time.Time); !ok || at.Sub(finding.CreatedAt).Abs() > time.Millisecond {
		t.Errorf("last_finding_at: got = %v, want %v", busyEntry["last_finding_at"], finding.CreatedAt)
	}
}

func TestAGangRetryGoesFromRunningToQueuedAndATerminalRowStays(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps := NewExperimentsStore(pool)
	ctx := context.Background()
	running, failed := "retry-running-"+peID, "retry-failed-"+peID
	seedExperimentForCapTest(t, pool, running, agentID, peID, 1, domain.StatusRunning)
	seedExperimentForCapTest(t, pool, failed, agentID, peID, 1, domain.StatusFailed)

	for i, want := range []bool{true, false} {
		got, err := exps.RequeueForRetry(ctx, running, 1)
		if err != nil || got != want {
			t.Fatalf("call %d on a RUNNING gang: RequeueForRetry = %v, %v, want %v", i+1, got, err, want)
		}
	}
	exp, err := exps.GetExperiment(ctx, running)
	if err != nil || exp.Status != domain.StatusQueued || exp.AttemptCount != 1 {
		t.Fatalf("after the retry: status %s, attempt %d, err %v, want QUEUED and 1", exp.Status, exp.AttemptCount, err)
	}
	if got, err := exps.RequeueForRetry(ctx, failed, 1); err != nil || got {
		t.Fatalf("RequeueForRetry of a FAILED row = %v, %v, want false", got, err)
	}
}
