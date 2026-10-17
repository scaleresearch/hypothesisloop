package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A stream that can announce a state the database then threw away is worse than no stream at
// all: an agent would act on a job it believes exists, and no later change would correct it. A
// change commits with its write or not at all, and once committed reaches a listener with the
// moment its write was made.
func TestAChangeIsAppendedWithItsWriteOrNotAtAll(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, hypothesisID := seedRun(t, pool, "rollback", true)
	store := NewStore(pool, 3)
	filter := domain.EventFilter{PlatformExperimentID: peID}
	comment := domain.Change{Kind: platformapi.EventCommentNew, Subject: hypothesisID, PlatformExperimentID: peID, AgentID: agentID}
	before := cursorNow(t, pool)
	events, stop := listenForEvents(t, pool)
	defer stop()

	refused := errors.New("the write failed")
	if err := store.Around(ctx, peID, func(a domain.Announcer) error {
		a.Change(comment)
		return refused
	}); !errors.Is(err, refused) {
		t.Fatalf("a write that fails: got = %v, want = %v", err, refused)
	}
	if got := changesAfter(t, store, filter, before); len(got) != 0 {
		t.Fatalf("changes after a rolled-back write: got = %+v, want = none", got)
	}

	started := time.Now().Add(-time.Second)
	if err := store.Around(ctx, peID, func(a domain.Announcer) error {
		a.Change(comment)
		return nil
	}); err != nil {
		t.Fatalf("a write that commits: got = %v, want = nil", err)
	}
	got := changesAfter(t, store, filter, before)
	if len(got) != 1 || got[0].ID <= before || got[0].Kind != platformapi.EventCommentNew || got[0].AgentID != agentID {
		t.Fatalf("changes after a committed write: got = %+v, want = the comment once, past %d", got, before)
	}
	if at := got[0].OccurredAt; at.Before(started) || at.After(time.Now().Add(time.Second)) || at.Location() != time.UTC {
		t.Errorf("occurred_at = %v, want the moment of the write, in UTC", at)
	}
	payload, err := json.Marshal(got[0])
	if err != nil || !strings.Contains(string(payload), `"occurred_at":"`+got[0].OccurredAt.Format(time.RFC3339Nano)+`"`) {
		t.Errorf("delivered change = %s, %v, want occurred_at in RFC3339 UTC", payload, err)
	}
	if live := awaitEvent(t, events, platformapi.EventCommentNew, hypothesisID); live != got[0] {
		t.Errorf("listened: got = %+v, want = %+v", live, got[0])
	}
}

// A job is announced as each write moves it. A job nobody has judged yet has no queue reason to
// report, and a reason with more to say is its code and its sentence.
func TestAJobsWritesAnnounceItsStatusesAndTheReasonsItWaitsFor(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, hypothesisID := seedRun(t, pool, "job", true)
	store := NewStore(pool, 3)
	const jobID = "watch-test-job-statuses"
	before := cursorNow(t, pool)
	queueJob(t, pool, jobOf(agentID, peID, hypothesisID, jobID, ""))
	const sentence = "cluster box-b is short of 16 cores, held by 2 jobs: 1 of this platform experiment and 1 of other platform experiments"
	if err := store.UpdateNotAdmittedReason(ctx, jobID, "capacity_unavailable: "+sentence); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE experiments SET status = 'SUBMITTED', not_admitted_reason = NULL WHERE id = $1`, jobID)
	if ok, err := store.MarkStarted(ctx, jobID); err != nil || !ok {
		t.Fatalf("MarkStarted: got = %v, %v, want = true", ok, err)
	}
	if ok, err := store.TransitionStatus(ctx, jobID, domain.StatusRunning, domain.StatusCompleted, ""); err != nil || !ok {
		t.Fatalf("TransitionStatus: got = %v, %v, want = true", ok, err)
	}

	var got []string
	for _, e := range changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID}, before) {
		if e.Subject == jobID {
			got = append(got, e.Kind+" "+e.Value+" "+e.Detail)
		}
	}
	want := []string{
		platformapi.EventExperimentStatus + " QUEUED ",
		platformapi.EventExperimentBlocked + " capacity_unavailable " + sentence,
		platformapi.EventExperimentStatus + " RUNNING ",
		platformapi.EventExperimentStatus + " COMPLETED ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes of the job:\ngot  = %q\nwant = %q", got, want)
	}
	job, err := store.GetExperiment(ctx, jobID)
	if err != nil || job.NotAdmittedReason != "" {
		t.Errorf("the job read: got = %+v, %v", job, err)
	}
}

// A job that ends by running is announced with the cause the write that ended it stored: a
// failure with how its process ended, a completion with nothing, and a cancelled job as a read
// answers it.
func TestAFinishedJobIsAnnouncedWithItsCause(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, hypothesisID := seedRun(t, pool, "cause", true)
	store := NewStore(pool, 3)
	const cause = "container_failed: command exited with code 1"
	for _, tc := range []struct {
		job, value, detail string
		end                func(id string) error
	}{
		{"watch-test-job-failed", "FAILED", cause, func(id string) error {
			_, err := store.TransitionStatus(ctx, id, domain.StatusRunning, domain.StatusFailed, cause)
			return err
		}},
		{"watch-test-job-completed", "COMPLETED", "", func(id string) error {
			_, err := store.TransitionStatus(ctx, id, domain.StatusRunning, domain.StatusCompleted, "")
			return err
		}},
		{"watch-test-job-cancelled", "CANCELLED", "cancelled", func(id string) error {
			_, err := store.ResolveTermination(ctx, id, domain.StatusRunning, domain.StatusEvicted, "cancelled")
			return err
		}},
	} {
		queueJob(t, pool, jobOf(agentID, peID, hypothesisID, tc.job, ""))
		exec(t, pool, `UPDATE experiments SET status = 'RUNNING', not_admitted_reason = NULL WHERE id = $1`, tc.job)
		before := cursorNow(t, pool)
		if err := tc.end(tc.job); err != nil {
			t.Fatalf("end %s: got = %v, want = nil", tc.job, err)
		}
		got := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{platformapi.EventExperimentStatus: true}}, before)
		if len(got) != 1 || got[0].Subject != tc.job || got[0].Value != tc.value || got[0].Detail != tc.detail {
			t.Errorf("end of %s: got = %+v, want %s with detail %q", tc.job, got, tc.value, tc.detail)
		}
	}
	if _, err := store.TransitionStatus(ctx, "watch-test-job-completed", domain.StatusCompleted, domain.StatusCompleted, cause); err == nil || !strings.Contains(err.Error(), "experiments_reason_of_attempt") {
		t.Errorf("a completion written with a reason: err = %v, want experiments_reason_of_attempt", err)
	}
}

// The description is the question every agent in the run is working on, and the duration is the
// time each plans its work by. A change of either says only that the brief changed: a description
// is unbounded, and copying it onto the stream would make a change a second copy of a row anyone
// can already GET. A write that changes neither says nothing.
func TestEditingTheBriefAnnouncesThatItChangedAndCarriesNoneOfItsText(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	_, peID, _ := seedRun(t, pool, "brief", true)
	store := NewStore(pool, 3)
	before := cursorNow(t, pool)
	edit := func(change func(pe *domain.PlatformExperiment)) {
		t.Helper()
		pe, err := store.GetPlatformExperiment(ctx, peID)
		if err != nil {
			t.Fatal(err)
		}
		change(pe)
		if err := store.UpdatePlatformExperiment(ctx, pe, pe.Status, nil); err != nil {
			t.Fatalf("update: got = %v, want = nil", err)
		}
	}
	edit(func(pe *domain.PlatformExperiment) {
		pe.Description = "the question, restated at some length after the coordinator resolved it"
	})
	edit(func(pe *domain.PlatformExperiment) { pe.DurationHours += 2 })
	edit(func(pe *domain.PlatformExperiment) {})

	got := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID}, before)
	if len(got) != 2 {
		t.Fatalf("changes: got = %+v, want = two description changes", got)
	}
	for _, e := range got {
		if e.Kind != platformapi.EventPlatformExperimentDescription || e.Subject != peID || e.Value != "" || e.Detail != "" {
			t.Errorf("description change: got = %+v, want = a pointer only", e)
		}
	}
}

// An agent is told to stop when the run closes. The reason an owner gave to close it is the close
// change's detail, and the platform experiment carries it.
func TestTheCloseIsAnnouncedWithTheReasonGiven(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	_, peID, _ := seedRun(t, pool, "closed-for", true)
	store := NewStore(pool, 3)
	before := cursorNow(t, pool)

	if err := store.ClosePlatformExperiment(ctx, peID, &domain.Actor{Kind: domain.ActorUser, ID: "u-1"}, "the baseline measured the old split"); err != nil {
		t.Fatal(err)
	}
	if err := store.ClosePlatformExperiment(ctx, peID, nil, "again"); err != nil {
		t.Fatal(err)
	}
	got := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID}, before)
	if len(got) != 1 || got[0].Kind != platformapi.EventPlatformExperimentStatus || got[0].Value != "closed" || got[0].Detail != "the baseline measured the old split" {
		t.Fatalf("changes: got = %+v, want = closed with the reason, once", got)
	}
	pe, err := store.GetPlatformExperiment(ctx, peID)
	if err != nil || pe.CloseMessage != "the baseline measured the old split" {
		t.Fatalf("close_message: got = %+v, %v", pe, err)
	}
}

// A status_reason is derived on every read, so a run whose last researcher's machine stopped for
// good read researchers_done and wrote no row: nothing woke the coordinator that was to close it.
// The reason that waits for the coordinator is recorded, and that write announces the status,
// once for each change, and never a reason of a status the row has left.
func TestAReasonThatWaitsForTheCoordinatorIsAnnouncedOnceAsAStatusChange(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	_, peID, _ := seedRun(t, pool, "coordinator-reason", true)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	store := NewStore(pool, 3)
	filter := domain.EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{platformapi.EventPlatformExperimentStatus: true}}
	tell := func(reasons ...string) {
		t.Helper()
		for _, reason := range reasons {
			if err := store.TellCoordinator(ctx, peID, reason); err != nil {
				t.Fatalf("tell %q: got = %v, want = nil", reason, err)
			}
		}
	}
	before := cursorNow(t, pool)

	tell("", "researchers_done", "researchers_done")
	// A researcher whose machine is asked for again leaves the run waiting for nobody, and the
	// next time none is left is told again.
	tell("", "researchers_done")
	got := changesAfter(t, store, filter, before)
	if len(got) != 2 || got[0].Value != "running" || got[0].Detail != "researchers_done" || got[1].Detail != "researchers_done" {
		t.Fatalf("changes: got = %+v, want = running researchers_done, twice", got)
	}

	if err := store.ClosePlatformExperiment(ctx, peID, nil, ""); err != nil {
		t.Fatal(err)
	}
	before = cursorNow(t, pool)
	tell("researchers_done")
	if got := changesAfter(t, store, filter, before); len(got) != 0 {
		t.Fatalf("telling a closed one: got = %+v, want = nothing", got)
	}
}

// A grant was announced as its guaranteed and burst hours added up: 8.96 where the agent could
// spend 2.24, of a budget of 6. It is announced as the share GET /quota answers.
func TestAQuotaGrantIsAnnouncedAsTheShareTheAgentMaySpend(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name              string
		guaranteed, burst float64
	}{{"quota-share", 2.24, 6.72}, {"quota-burst-only", 0, 4.5}} {
		agentID, peID, _ := seedRun(t, pool, tc.name, true)
		before := cursorNow(t, pool)
		q := &domain.AgentQuota{ID: agentID, AgentID: agentID, PlatformExperimentID: peID,
			GuaranteedAcceleratorHours: tc.guaranteed, BurstAcceleratorHours: tc.burst, CreatedAt: time.Now()}
		if err := pool.write(ctx, func(w *Write) error { return upsertAgentQuota(ctx, w, q) }); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { exec(t, pool, `DELETE FROM agent_quotas WHERE id = $1`, agentID) })

		want := fmt.Sprintf("%g", q.Share())
		got := changesAfter(t, NewStore(pool, 3), domain.EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{platformapi.EventQuotaChanged: true}}, before)
		if len(got) != 1 || got[0].Subject != agentID || got[0].Value != want || got[0].Detail != "" {
			t.Fatalf("%s: changes: got = %+v, want = the share %s and no detail", tc.name, got, want)
		}
	}
}

// One cut row says two different things to two different audiences: the ladder moved, which is
// news for everyone, and this agent is done, which only that agent can act on. The agent's other
// way out is its own: it says it has nothing left to do.
func TestACutAnnouncesBothTheLadderBoundaryAndTheCutAgentsOwnStopCondition(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, _ := seedRun(t, pool, "cut", true)
	store := NewStore(pool, 3)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type) VALUES ($1, $2, 'guaranteed', 'staff', 'generalist')`, peID, agentID)
	before := cursorNow(t, pool)

	if err := store.FinishAgent(ctx, peID, agentID, "quota spent"); err != nil {
		t.Fatalf("finish: got = %v, want = nil", err)
	}
	if err := pool.write(ctx, func(w *Write) error { return cutTx(ctx, w, peID, []string{agentID}, 1, "") }); err != nil {
		t.Fatalf("cut: got = %v, want = nil", err)
	}
	if err := pool.write(ctx, func(w *Write) error { return cutTx(ctx, w, peID, []string{agentID}, 1, "") }); err != nil {
		t.Fatalf("cut again: got = %v, want = nil", err)
	}

	var got []string
	for _, e := range changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID}, before) {
		got = append(got, e.Kind+" "+e.Subject+" "+e.Value+" "+e.Detail)
	}
	want := []string{
		platformapi.EventAgentFinished + " " + agentID + "  quota spent",
		platformapi.EventStageBoundary + " " + peID + " 1 cut",
		platformapi.EventAgentCut + " " + agentID + " 1 ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes:\ngot  = %q\nwant = %q", got, want)
	}
}

// The default subscription exists so an agent does not have to know the vocabulary to stop
// polling, so it carries every kind on the stream.
func TestTheDefaultKindSetIsEveryKindOnTheStream(t *testing.T) {
	kinds := domain.DefaultKinds()
	for _, kind := range domain.EventKinds {
		if !kinds[kind.Kind] {
			t.Errorf("%v in the default set: got = %v, want = %v", kind.Kind, false, true)
		}
	}
}

// Naming kinds means exactly those kinds.
func TestAnExplicitKindSetMeansExactlyWhatItNames(t *testing.T) {
	filter := domain.EventFilter{PlatformExperimentID: "pe-1", Kinds: map[string]bool{platformapi.EventCommentNew: true}}
	if !filter.Matches(domain.Event{Change: domain.Change{Kind: platformapi.EventCommentNew, Subject: "hyp-1", PlatformExperimentID: "pe-1"}}) {
		t.Errorf("comment.new on a subscription that named it: got = %v, want = %v", false, true)
	}
	if filter.Matches(domain.Event{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Subject: "exp-1", PlatformExperimentID: "pe-1"}}) {
		t.Errorf("experiment.status on a subscription that named only comment.new: got = %v, want = %v", true, false)
	}
}

// An agent's stream leaves out a queue reason that waiting clears, so an agent whose job never
// started learned nothing. A job that has waited longer than it was to run, and longer than the
// platform's minimum, is announced once with the reason it has then, to its own agent and to
// whoever steers the run.
func TestAJobThatWaitsLongerThanItWasToRunIsAnnouncedAsHeldOnce(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, hypothesisID := seedRun(t, pool, "held", true)
	store := NewStore(pool, 3)
	const sentence = "cluster box-b is short of 1 x tenstorrent.com/chiparch=blackhole, held by 1 job of other platform experiments"
	queue := func(id, reason string, estimatedHours float64, waited time.Duration) {
		t.Helper()
		exp := jobOf(agentID, peID, hypothesisID, id, reason)
		exp.EstimatedDurationHours, exp.CreatedAt = estimatedHours, time.Now().Add(-waited)
		queueJob(t, pool, exp)
	}
	queue("held-job-long-wait", "capacity_unavailable: "+sentence, 0.08, 10*time.Minute)
	queue("held-job-short-wait", "capacity_unavailable: "+sentence, 0.08, 4*time.Minute)
	queue("held-job-long-run", "capacity_unavailable: "+sentence, 1, 10*time.Minute)
	queue("held-job-unjudged", domain.QueueReasonAwaitingAdmission, 0.08, 10*time.Minute)
	before := cursorNow(t, pool)

	if err := store.MarkHeldJobs(ctx, 5*time.Minute); err != nil {
		t.Fatalf("MarkHeldJobs: %v", err)
	}
	// A second pass, and a reason that changed since, announce nothing more.
	exec(t, pool, `UPDATE experiments SET not_admitted_reason = 'outranked' WHERE id = 'held-job-long-wait'`)
	if err := store.MarkHeldJobs(ctx, 5*time.Minute); err != nil {
		t.Fatalf("MarkHeldJobs: %v", err)
	}
	got := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{platformapi.EventJobHeld: true}}, before)
	if len(got) != 1 || got[0].Subject != "held-job-long-wait" || got[0].Value != "capacity_unavailable" || got[0].Detail != sentence || got[0].AgentID != agentID {
		t.Fatalf("held changes: got = %+v, want = the one job that waited past its estimate and the minimum, once", got)
	}
	held := got[0]
	if !(domain.EventFilter{PlatformExperimentID: peID, AgentID: agentID}).Matches(held) || !(domain.EventFilter{PlatformExperimentID: peID, Decisions: true}).Matches(held) {
		t.Errorf("a held job is kept from its agent or from its coordinator: %+v", held)
	}
	if (domain.EventFilter{PlatformExperimentID: peID, AgentID: "another-agent"}).Matches(held) {
		t.Errorf("a held job is announced to another agent: %+v", held)
	}
}

// A baseline job sat building its environment past the point it could end by
// baseline_due_at, and its agent, whose stream leaves out a queue reason that waiting clears,
// slept through it. Such a job is announced to its agent once, as job.held baseline_late.
func TestABaselineJobThatCannotEndByItsDeadlineIsAnnouncedOnce(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, hypothesisID := seedRun(t, pool, "late", true)
	store := NewStore(pool, 3)
	queueJob(t, pool, jobOf(agentID, peID, hypothesisID, "late-job", "environment_building: the platform builds it"))
	before := cursorNow(t, pool)
	const first = "job late-job is QUEUED (environment_building): it cannot end by baseline_due_at"
	if err := store.MarkBaselineLate(ctx, "late-job", domain.HeldBaselineLate+": "+first); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBaselineLate(ctx, "late-job", domain.HeldBaselineLate+": a minute later, still late"); err != nil {
		t.Fatal(err)
	}
	got := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, Kinds: map[string]bool{platformapi.EventJobHeld: true}}, before)
	if len(got) != 1 || got[0].Subject != "late-job" || got[0].Value != domain.HeldBaselineLate || got[0].Detail != first || got[0].AgentID != agentID {
		t.Fatalf("held changes of a late baseline job: got = %+v, want baseline_late with its sentence to its agent, once", got)
	}
	var reason string
	if err := pool.pool.QueryRow(ctx, `SELECT held_reason FROM experiments WHERE id = 'late-job'`).Scan(&reason); err != nil || reason != domain.HeldBaselineLate+": "+first {
		t.Errorf("held_reason after a second notice = %q, %v, want the first one kept", reason, err)
	}
	if err := store.MarkBaselineLate(ctx, "late-job", "no sentence"); err == nil {
		t.Error("a reason with no sentence was taken")
	}
}

// Two researchers ran one hypothesis and nobody noticed. A job submitted with the other agents'
// jobs on its hypothesis keeps the sentence its submitter was told, and whoever steers the run is
// told it as job.duplicate, once, with no other agent.
func TestAJobOnAnotherAgentsHypothesisIsAnnouncedAsADuplicateOnce(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	agentID, peID, hypothesisID := seedRun(t, pool, "duplicate", true)
	store := NewStore(pool, 3)
	before := cursorNow(t, pool)
	const sentence = "hypothesis h is tested by another agent too: job-a of Researcher 2 (RUNNING)"
	exp := jobOf(agentID, peID, hypothesisID, "duplicate-job", "")
	exp.SameHypothesis = sentence
	queueJob(t, pool, exp)
	if err := store.UpdateNotAdmittedReason(ctx, "duplicate-job", "outranked: a job ranked higher"); err != nil {
		t.Fatal(err)
	}

	got := changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, Decisions: true, Kinds: map[string]bool{platformapi.EventJobDuplicate: true}}, before)
	if len(got) != 1 || got[0].Subject != "duplicate-job" || got[0].Value != hypothesisID || got[0].Detail != sentence || got[0].AgentID != agentID {
		t.Fatalf("duplicate changes: got = %+v, want the hypothesis, the sentence and the job's agent, once", got)
	}
	if (domain.EventFilter{PlatformExperimentID: peID, AgentID: "another-agent"}).Matches(got[0]) {
		t.Errorf("a duplicate is announced to another agent: %+v", got[0])
	}
	job, err := store.GetExperiment(ctx, "duplicate-job")
	if err != nil || job == nil || job.SameHypothesis != sentence {
		t.Fatalf("the job as read: %+v, %v, want same_hypothesis kept", job, err)
	}
}

// An agent scope keeps what the agent must act on: its own jobs' outcomes, the queue reasons that
// waiting will not clear, its allocation and cut. A rival's are private state and would wake it
// for nothing, and a job moving through QUEUED, SUBMITTED or RUNNING, or waiting in the queue for
// capacity, asks nothing of it.
func TestAnAgentScopeAdmitsItsOwnOutcomesAndNoRivalsOrIntermediateStatuses(t *testing.T) {
	filter := domain.EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a", Kinds: domain.DefaultKinds()}
	admitted := []domain.Event{
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "EVICTED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "QUEUED", Detail: "stuck_pending: admitted and not running within 5m0s", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "exceeds_accelerator_share: cpu 64 above 8", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "exceeds_cluster_capacity: needs {accelerator:h200=3}, more than any connected cluster has installed", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventQuotaChanged, Value: "4", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventAgentCut, Value: "1", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventStageBoundary, Value: "1", Detail: "advanced"}},
		{Change: domain.Change{Kind: platformapi.EventPlatformExperimentStatus, Value: "closed"}},
		{Change: domain.Change{Kind: platformapi.EventPlatformExperimentDescription}},
		{Change: domain.Change{Kind: platformapi.EventCommentNew, AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventCommentNew}},
	}
	refused := []domain.Event{
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "QUEUED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "SUBMITTED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "RUNNING", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "capacity_unavailable", Detail: "cluster c is short of 1 x nvidia.com/gpu.product=nvidia-l40", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "outranked", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "concurrency_cap", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventQuotaChanged, Value: "4", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventAgentCut, Value: "1", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventStageBoundary, Value: "1", Detail: "cut", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventStageBoundary, Value: "1", Detail: "cut", AgentID: "agent-b"}},
	}
	for _, e := range admitted {
		e.PlatformExperimentID = "pe-1"
		if !filter.Matches(e) {
			t.Errorf("Matches(%+v): got = false, want = true", e)
		}
	}
	for _, e := range refused {
		e.PlatformExperimentID = "pe-1"
		if filter.Matches(e) {
			t.Errorf("Matches(%+v): got = true, want = false", e)
		}
	}
}

// The agent's own writes to the pool are echoes of what it just did; waking it for them is spam.
func TestAnAgentScopeDropsTheEchoOfItsOwnHypothesisActivity(t *testing.T) {
	filter := domain.EventFilter{PlatformExperimentID: "pe-1", AgentID: "agent-a", Kinds: domain.DefaultKinds()}
	for _, kind := range []string{platformapi.EventHypothesisNew, platformapi.EventHypothesisStatus, platformapi.EventHypothesisLink, platformapi.EventFindingNew, platformapi.EventCommentNew} {
		if filter.Matches(domain.Event{Change: domain.Change{Kind: kind, Subject: "hyp-1", PlatformExperimentID: "pe-1", AgentID: "agent-a"}}) {
			t.Errorf("own %v on an agent scope: got = true, want = false", kind)
		}
	}
}

// Without an agent the stream is the whole platform experiment, whoever wrote each row.
func TestASubscriptionWithoutAnAgentCarriesEveryAgentsEvents(t *testing.T) {
	filter := domain.EventFilter{PlatformExperimentID: "pe-1", Kinds: domain.DefaultKinds()}
	for _, e := range []domain.Event{
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "RUNNING", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventHypothesisNew, AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventStageBoundary, Detail: "cut", AgentID: "agent-b"}},
	} {
		e.PlatformExperimentID = "pe-1"
		if !filter.Matches(e) {
			t.Errorf("Matches(%+v): got = false, want = true", e)
		}
	}
}

// A sandboxed coordinator was woken for every job being queued, admitted and started, each wake a
// reread of its whole context that ended in "nothing to act on". Whoever steers an experiment
// decides on outcomes and on jobs that are stuck, across every agent.
func TestTheDecisionsStreamOfAnExperimentLeavesOutJobsThatAreOnTheirWay(t *testing.T) {
	filter := domain.EventFilter{PlatformExperimentID: "pe-1", Kinds: domain.DefaultKinds(), Decisions: true}
	whole := domain.EventFilter{PlatformExperimentID: "pe-1", Kinds: domain.DefaultKinds()}
	admitted := []domain.Event{
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "COMPLETED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "FAILED", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "EVICTED", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "job_too_long", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventHypothesisNew, AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventHypothesisStatus, Value: "confirmed", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventFindingNew, Value: "job-1", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventCommentNew}},
		{Change: domain.Change{Kind: platformapi.EventQuotaChanged, Value: "4", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventAgentCut, Value: "1", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventStageBoundary, Value: "1", Detail: "cut", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventPlatformExperimentStatus, Value: "closed"}},
	}
	onTheirWay := []domain.Event{
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "QUEUED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "SUBMITTED", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentStatus, Value: "RUNNING", AgentID: "agent-b"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "capacity_unavailable", Detail: "cluster c is short of 1 x nvidia.com/gpu.product=nvidia-l40", AgentID: "agent-a"}},
		{Change: domain.Change{Kind: platformapi.EventExperimentBlocked, Value: "outranked", AgentID: "agent-b"}},
	}
	for _, e := range admitted {
		e.PlatformExperimentID = "pe-1"
		if !filter.Matches(e) {
			t.Errorf("Matches(%+v): got = false, want = true", e)
		}
	}
	for _, e := range onTheirWay {
		e.PlatformExperimentID = "pe-1"
		if filter.Matches(e) {
			t.Errorf("Matches(%+v): got = true, want = false", e)
		}
		if !whole.Matches(e) {
			t.Errorf("the whole stream lost %+v, which only the decisions stream leaves out", e)
		}
	}
}

// dueOf is the researchers of peID due an idle notice: other tests leave runs of their own.
func dueOf(t *testing.T, store *Store, peID string, after time.Duration) ([]IdleResearcher, error) {
	t.Helper()
	all, err := store.IdleResearchers(context.Background(), after)
	var due []IdleResearcher
	for _, r := range all {
		if r.PlatformExperimentID == peID {
			due = append(due, r)
		}
	}
	return due, err
}

// noticeIdle tells every researcher of peID due an idle notice, as the service does, with a
// detail that says the minutes of its stretch, and answers how many were told.
func noticeIdle(t *testing.T, store *Store, peID string, after time.Duration) int {
	t.Helper()
	due, err := store.IdleResearchers(context.Background(), after)
	if err != nil {
		t.Fatal(err)
	}
	told := 0
	for _, r := range due {
		if r.PlatformExperimentID != peID {
			continue
		}
		if err := store.NoticeIdle(context.Background(), r, fmt.Sprintf("idle %d minutes, notice %d", int(time.Since(r.Since).Minutes()), r.Repeat)); err != nil {
			t.Fatal(err)
		}
		told++
	}
	return told
}

// Two passes that read the same due researcher tell it once: the second write finds the notice
// the first one wrote.
func TestAnIdleNoticeToldSinceItWasReadStands(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, _ := seedRun(t, pool, "idle-race", true)
	store := NewStore(pool, 3)
	ctx := context.Background()
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', started_at = now() - interval '20 minutes' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type, signed_up_at) VALUES ($1, $2, 'guaranteed', 'staff', 'generalist', now() - interval '30 minutes')`, peID, agentID)
	due, err := dueOf(t, store, peID, 10*time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("due %+v, %v, want the one researcher", due, err)
	}
	if err := store.NoticeIdle(ctx, due[0], "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.NoticeIdle(ctx, due[0], "second"); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := pool.pool.QueryRow(ctx, `SELECT idle_detail FROM experiment_signups WHERE platform_experiment_id = $1 AND agent_id = $2`, peID, agentID).Scan(&detail); err != nil || detail != "first" {
		t.Fatalf("detail %q, %v, want the first notice kept", detail, err)
	}
}

// A job running is activity, and the stretch starts when the last one ended. The roster says
// what the researcher has in flight, and for how long it had none.
func TestAResearcherWithAJobRunningIsNotIdle(t *testing.T) {
	pool := eventsTestDB(t)
	agentID, peID, hypothesisID := seedRun(t, pool, "running", true)
	store := NewStore(pool, 3)
	ctx := context.Background()
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', started_at = now() - interval '40 minutes' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type, signed_up_at) VALUES ($1, $2, 'guaranteed', 'staff', 'generalist', now() - interval '40 minutes')`, peID, agentID)
	job := func(id, status, created, reported string) {
		t.Helper()
		exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, data_access_key_id, data_secret_access_key,
 created_at, run_reported_at)
VALUES ('agent', $4, $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator', $5, 1, 'AK', 'SK',
        now() - $6::interval, now() - $7::interval)`, agentID, peID, hypothesisID, id, status, created, reported)
	}
	inFlight := func() domain.InFlight {
		t.Helper()
		signups, err := store.ListAgentSignups(ctx, peID)
		if err != nil || len(signups) != 1 || signups[0].InFlight == nil {
			t.Fatalf("roster: %+v, %v", signups, err)
		}
		return *signups[0].InFlight
	}

	job("job-running", "RUNNING", "30 minutes", "30 minutes")
	job("job-queued", "SUBMITTED", "5 minutes", "5 minutes")
	if told := noticeIdle(t, store, peID, 0); told != 0 {
		t.Fatalf("a job running: told %d, want no notice", told)
	}
	if got := inFlight(); got.Jobs != 2 || got.Running != 1 || got.Queued != 1 || got.IdleSince != nil || *got.MinutesWithoutJob(time.Now()) != 0 {
		t.Fatalf("jobs in flight: in_flight %+v, want one running and one queued, and 0 minutes without", got)
	}
	exec(t, pool, `UPDATE experiments SET status = 'COMPLETED', run_reported_at = now() - interval '210 seconds' WHERE id = 'job-running'`)
	exec(t, pool, `UPDATE experiments SET status = 'FAILED', created_at = now() - interval '30 minutes', run_reported_at = NULL WHERE id = 'job-queued'`)
	if told := noticeIdle(t, store, peID, 10*time.Minute); told != 0 {
		t.Fatalf("a job that ended 3 minutes ago, notice at 10: told %d, want no notice", told)
	}
	due, err := dueOf(t, store, peID, 2*time.Minute)
	if err != nil || len(due) != 1 || int(time.Since(due[0].Since).Minutes()) != 3 || due[0].Repeat != 0 {
		t.Fatalf("notice at 2: due %+v, %v, want the 3 minutes since the job ended, a first notice", due, err)
	}
	got := inFlight()
	if got.Jobs != 0 || got.IdleSince == nil || *got.MinutesWithoutJob(time.Now()) != 3 {
		t.Fatalf("idle: in_flight %+v, want nothing in flight for 3 minutes", got)
	}
}

// A researcher worked 28 minutes on its own machine with nothing on the platform, and neither
// it nor the coordinator was told. A lead told once rested from mid-run to the close. A researcher with no job submitted for the notice and none in flight is told so, new, and
// again every notice while the stretch lasts, continues.
func TestAResearcherWithNoJobForTheNoticeIsToldAgainWhileTheStretchLasts(t *testing.T) {
	pool := eventsTestDB(t)
	before := cursorNow(t, pool)
	agentID, peID, hypothesisID := seedRun(t, pool, "idle", true)
	store := NewStore(pool, 3)
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', started_at = now() - interval '20 minutes' WHERE id = $1`, peID)
	exec(t, pool, `INSERT INTO experiment_signups (platform_experiment_id, agent_id, quota_tier, added_by_kind, agent_type, signed_up_at) VALUES ($1, $2, 'guaranteed', 'staff', 'generalist', now() - interval '30 minutes')`, peID, agentID)
	announced := func() []domain.Event {
		t.Helper()
		return changesAfter(t, store, domain.EventFilter{PlatformExperimentID: peID, AgentID: agentID, Kinds: map[string]bool{platformapi.EventAgentIdle: true}}, before)
	}
	job := func(id, status string) {
		t.Helper()
		exec(t, pool, `INSERT INTO experiments
(actor_kind, id, agent_id, platform_experiment_id, code_ref, config_hash, data_ref,
 hypothesis_id, hypothesis, accelerator_type, status, estimated_duration_hours, data_access_key_id, data_secret_access_key)
VALUES ('agent', $4, $1, $2, 'r@0', 'h', 'd', $3, 'claim', 'test-accelerator', $5, 1, 'AK', 'SK')`, agentID, peID, hypothesisID, id, status)
	}
	// The notice before this one is moved back by ago, as the clock would move it.
	noticedAgo := func(ago string) {
		t.Helper()
		exec(t, pool, `UPDATE experiment_signups SET idle_noticed_at = idle_noticed_at - $3::interval WHERE platform_experiment_id = $1 AND agent_id = $2`, peID, agentID, ago)
	}

	if told := noticeIdle(t, store, peID, 30*time.Minute); told != 0 {
		t.Fatalf("idle 20 minutes, notice at 30: told %d, want none", told)
	}
	events, stop := listenForEvents(t, pool)
	if told := noticeIdle(t, store, peID, 10*time.Minute); told != 1 {
		t.Fatalf("idle 20 minutes, notice at 10: told %d, want once", told)
	}
	live := awaitEvent(t, events, platformapi.EventAgentIdle, agentID)
	stop()
	if live.Value != domain.IdleNew || live.Detail != "idle 20 minutes, notice 0" || live.PlatformExperimentID != peID {
		t.Fatalf("live: got = %+v, want new with the 20 minutes since the run started", live)
	}
	if told := noticeIdle(t, store, peID, 10*time.Minute); told != 0 {
		t.Fatalf("the same stretch within the notice: told %d, want nothing more", told)
	}
	noticedAgo("11 minutes")
	events, stop = listenForEvents(t, pool)
	if told := noticeIdle(t, store, peID, 10*time.Minute); told != 1 {
		t.Fatalf("the same stretch a notice later: told %d, want once more", told)
	}
	live = awaitEvent(t, events, platformapi.EventAgentIdle, agentID)
	stop()
	if live.Value != domain.IdleContinues || live.Detail != "idle 20 minutes, notice 1" {
		t.Fatalf("live repeat: got = %+v, want continues, the second notice of the stretch", live)
	}
	if got := announced(); len(got) != 2 || got[0].Value != domain.IdleNew || got[1].Value != domain.IdleContinues {
		t.Fatalf("changes: %+v, want new then continues", got)
	}
	noticedAgo("11 minutes")
	if told := noticeIdle(t, store, peID, 10*time.Minute); told != 1 {
		t.Fatalf("a third notice: told %d, want once more", told)
	}

	job("idle-job-1", "RUNNING")
	noticedAgo("11 minutes")
	if told := noticeIdle(t, store, peID, 0); told != 0 {
		t.Fatalf("a job running: told %d, want nothing", told)
	}
	exec(t, pool, `UPDATE experiments SET status = 'COMPLETED' WHERE id = 'idle-job-1'`)
	if told := noticeIdle(t, store, peID, 10*time.Minute); told != 0 {
		t.Fatalf("its job just ended: told %d, want nothing", told)
	}
	if told := noticeIdle(t, store, peID, 0); told != 1 {
		t.Fatalf("a new stretch: told %d, want once", told)
	}
	if got := announced(); len(got) != 4 || got[3].Value != domain.IdleNew || got[3].Detail != "idle 0 minutes, notice 0" {
		t.Fatalf("a new stretch: changes %+v, want its first notice, new, last", got)
	}

	exec(t, pool, `UPDATE experiment_signups SET finished_at = now() WHERE platform_experiment_id = $1`, peID)
	exec(t, pool, `UPDATE experiment_signups SET idle_noticed_at = NULL WHERE platform_experiment_id = $1`, peID)
	if told := noticeIdle(t, store, peID, 0); told != 0 {
		t.Fatalf("a finished researcher: told %d, want none", told)
	}
}

// A run's places stood free while its researchers had jobs in flight, and its coordinator was not
// told. Places free for the notice are told, new, and again every notice while they stay free,
// continues, to whoever steers the run and to no agent. A pass that sees none
// free ends the stretch, and the next one starts over.
func TestARunWithPlacesFreeForTheNoticeIsToldAgainWhileTheyStayFree(t *testing.T) {
	pool := eventsTestDB(t)
	before := cursorNow(t, pool)
	agentID, peID, _ := seedRun(t, pool, "capacity-idle", true)
	store := NewStore(pool, 3)
	ctx := context.Background()
	exec(t, pool, `UPDATE platform_experiments SET status = 'running', started_at = now() - interval '1 hour', ends_at = now() + interval '3 hours' WHERE id = $1`, peID)
	mark := func(free bool, after time.Duration) *IdleCapacity {
		t.Helper()
		due, err := store.MarkCapacityFree(ctx, peID, free, after)
		if err != nil {
			t.Fatal(err)
		}
		return due
	}
	announced := func(filter domain.EventFilter) []domain.Event {
		t.Helper()
		filter.PlatformExperimentID, filter.Kinds = peID, map[string]bool{platformapi.EventCapacityIdle: true}
		return changesAfter(t, store, filter, before)
	}
	// The stretch and the notice before this one are moved back by ago, as the clock would move them.
	ago := func(ago string) {
		t.Helper()
		exec(t, pool, `UPDATE platform_experiments SET capacity_free_since = capacity_free_since - $2::interval,
capacity_noticed_at = capacity_noticed_at - $2::interval WHERE id = $1`, peID, ago)
	}

	if due := mark(true, 10*time.Minute); due != nil {
		t.Fatalf("places just seen free: due %+v, want none before the notice", due)
	}
	ago("11 minutes")
	due := mark(true, 10*time.Minute)
	if due == nil || due.Repeat != 0 || due.NoticedAt != nil || time.Since(due.Since) < 10*time.Minute {
		t.Fatalf("free 11 minutes: due %+v, want the first notice of a stretch begun 11 minutes ago", due)
	}
	events, stop := listenForEvents(t, pool)
	if err := store.NoticeCapacityIdle(ctx, *due, "first"); err != nil {
		t.Fatal(err)
	}
	live := awaitEvent(t, events, platformapi.EventCapacityIdle, peID)
	stop()
	if live.Value != domain.IdleNew || live.Detail != "first" || live.PlatformExperimentID != peID || live.AgentID != "" {
		t.Fatalf("live: got = %+v, want new with its detail, of the run", live)
	}
	if err := store.NoticeCapacityIdle(ctx, *due, "told twice"); err != nil {
		t.Fatal(err)
	}
	if due := mark(true, 10*time.Minute); due != nil {
		t.Fatalf("the same stretch within the notice: due %+v, want nothing more", due)
	}
	ago("11 minutes")
	due = mark(true, 10*time.Minute)
	if due == nil || due.Repeat != 1 {
		t.Fatalf("the same stretch a notice later: due %+v, want the second notice", due)
	}
	if err := store.NoticeCapacityIdle(ctx, *due, "second"); err != nil {
		t.Fatal(err)
	}
	if got := announced(domain.EventFilter{Decisions: true}); len(got) != 2 || got[0].Detail != "first" || got[1].Value != domain.IdleContinues || got[1].Detail != "second" || got[1].Subject != peID {
		t.Fatalf("changes: %+v, want the first notice, then the second, continues", got)
	}
	if got := announced(domain.EventFilter{AgentID: agentID}); len(got) != 0 {
		t.Fatalf("an agent's changes: %+v, want none: it is the run's", got)
	}

	if due := mark(false, 0); due != nil {
		t.Fatalf("no place free: due %+v", due)
	}
	due = mark(true, 0)
	if due == nil || due.Repeat != 0 {
		t.Fatalf("places free again: due %+v, want the first notice of a new stretch", due)
	}

	exec(t, pool, `UPDATE platform_experiments SET status = 'closed' WHERE id = $1`, peID)
	if due := mark(true, 0); due != nil {
		t.Fatalf("a closed run: due %+v, want none", due)
	}
}
