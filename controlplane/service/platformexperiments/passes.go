package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// SweepExpired closes every platform experiment whose ClosesAt has passed: a run at its end, one
// no researcher's machine came up for by its AgentsDueAt, a draft whose planning limit passed or
// whose answer no person took up, which releases its machine, and an open one that is not running
// by its BaselineDueAt: the machines that were to measure its baseline would be rented for nothing.
func (s *Service) SweepExpired(ctx context.Context) error {
	return s.sweepExpired(ctx, domain.PlatformExpDraft, domain.PlatformExpOpen, domain.PlatformExpRunning)
}

// SweepEnded is SweepExpired for running platform experiments alone: a run's end is what its
// agents and its coordinator's last turn are timed by, so it is closed on time.
func (s *Service) SweepEnded(ctx context.Context) error {
	return s.sweepExpired(ctx, domain.PlatformExpRunning)
}

func (s *Service) sweepExpired(ctx context.Context, statuses ...domain.PlatformExperimentStatus) error {
	now := time.Now()
	var failed error
	for _, status := range statuses {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("list %s platform experiments: %w", status, err))
			continue
		}
		for _, pe := range pes {
			closesAt := pe.ClosesAt()
			if closesAt.IsZero() || closesAt.After(now) {
				continue
			}
			if _, err := s.Close(ctx, pe.ID, nil, "", nil); err != nil {
				s.logger.Error("sweep_expired: close failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
				continue
			}
			s.logger.Info("sweep_expired: closed platform experiment",
				zap.String("platform_experiment_id", pe.ID), zap.Time("closes_at", closesAt))
		}
	}
	return failed
}

// SweepAutoStart starts every open platform experiment that is to be started: its baseline is
// declared, its starts_at is absent or past, and it has a researcher and budget left for one.
// The request that declares the baseline or adds the researcher starts it itself (StartIfReady),
// so what the sweep brings is time: a starts_at that arrives. One with no baseline is given the
// job its baseline agents left, where the request they finished in could not declare it
// (DeclareMeasured). One that is not startable waits, saying why in its status_reason (Describe),
// until SweepExpired closes it.
func (s *Service) SweepAutoStart(ctx context.Context) error {
	now := time.Now().UTC()
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
		if pe.BaselineJobID == "" {
			if err := s.DeclareMeasured(ctx, pe.ID); err != nil {
				s.logger.Error("sweep_auto_start: declare the measured baseline failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			}
			continue
		}
		_, err := s.start(ctx, pe, now)
		var waiting *refusal.Refusal
		if errors.As(err, &waiting) {
			continue
		}
		if err != nil {
			s.logger.Error("sweep_auto_start: start failed",
				zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		s.logger.Info("sweep_auto_start: started platform experiment", zap.String("platform_experiment_id", pe.ID))
	}
	return nil
}

// TendBaselines keeps the clock of every open platform experiment with no baseline declared.
// Time the platform spends on a job of its baseline, building its environment or starting it on
// its cluster, is the platform's and not the baseline agent's: baseline_due_at moves out by it,
// at most domain.MaxEnvironmentBuildTime in all (db.HoldBaselineClock). And the agent of a job
// that, started now, ends after baseline_due_at is told so once, as job.held, which wakes it to
// act on it where status_reason alone would wait to be read.
func (s *Service) TendBaselines(ctx context.Context) error {
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
		if pe.BaselineJobID != "" {
			continue
		}
		if err := s.tendBaseline(ctx, pe); err != nil {
			s.logger.Error("tend_baselines: failed", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
		}
	}
	return nil
}

func (s *Service) tendBaseline(ctx context.Context, pe *domain.PlatformExperiment) error {
	jobs, err := s.store.ListExperimentsTesting(ctx, []string{pe.ID})
	if err != nil {
		return err
	}
	held := slices.ContainsFunc(jobs, waitsOnPlatform)
	if err := s.store.HoldBaselineClock(ctx, pe.ID, held, domain.MaxEnvironmentBuildTime); err != nil {
		return err
	}
	if held {
		if pe, err = s.stored(ctx, pe.ID); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	var failed error
	for _, job := range jobs {
		late := lateBaselineJob(job, pe.BaselineDueAt, now)
		if late == "" {
			continue
		}
		reason := domain.HeldBaselineLate + ": " + late + ". " + strings.TrimSpace(baselineClock(pe.BaselineDueAt, now))
		if err := s.store.MarkBaselineLate(ctx, job.ID, reason); err != nil {
			failed = errors.Join(failed, fmt.Errorf("job %s: %w", job.ID, err))
		}
	}
	return failed
}

// TellCoordinators records, for every open or running platform experiment whose coordinator the
// platform hosts, the status_reason it waits for that coordinator with, and none where it waits
// for someone else. A status_reason is derived from the roster and the clock and stored nowhere,
// so what moves it writes nothing a coordinator is woken by: a researcher's machine that stops
// with no restart left, a baseline agent's that does. The record is that write, and one that
// changes is announced as platform_experiment.status (db.TellCoordinator).
func (s *Service) TellCoordinators(ctx context.Context) error {
	var failed error
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("list %s platform experiments: %w", status, err))
			continue
		}
		for _, pe := range pes {
			if !pe.CoordinatorHosted {
				continue
			}
			if err := s.tellCoordinator(ctx, pe); err != nil {
				s.logger.Error("tell_coordinators: failed", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			}
		}
	}
	return failed
}

func (s *Service) tellCoordinator(ctx context.Context, pe *domain.PlatformExperiment) error {
	if err := s.Describe(ctx, pe, auth.Principal{}); err != nil {
		return err
	}
	reason := ""
	if pe.StatusReason.WaitsFor == domain.WaitsForCoordinator {
		reason = pe.StatusReason.Reason
	}
	return s.store.TellCoordinator(ctx, pe.ID, reason)
}

// StartExpirySweep runs SweepAutoStart, TendBaselines, SweepExpired, RecordCloses,
// TellCoordinators and the idle notices on a ticker until ctx is cancelled, each pass bounded by
// passTimeout. TendBaselines runs before SweepExpired, so a baseline that waited on the platform
// until its deadline is given that time before it is closed.
func (s *Service) StartExpirySweep(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, passTimeout)
			s.expirySweep(passCtx)
			cancel()
		}
	}
}

func (s *Service) expirySweep(ctx context.Context) {
	if err := s.SweepAutoStart(ctx); err != nil {
		s.logger.Error("sweep_auto_start: scan", zap.Error(err))
	}
	if err := s.TendBaselines(ctx); err != nil {
		s.logger.Error("tend_baselines: scan", zap.Error(err))
	}
	if err := s.SweepExpired(ctx); err != nil {
		s.logger.Error("sweep_expired: scan", zap.Error(err))
	}
	if err := s.RecordCloses(ctx); err != nil {
		s.logger.Error("record_closes: scan", zap.Error(err))
	}
	if err := s.TellCoordinators(ctx); err != nil {
		s.logger.Error("tell_coordinators: scan", zap.Error(err))
	}
	if s.idle != nil {
		if err := s.NoticeIdleResearchers(ctx); err != nil {
			s.logger.Error("notice_idle_researchers: scan", zap.Error(err))
		}
		if err := s.NoticeIdleCapacity(ctx); err != nil {
			s.logger.Error("notice_idle_capacity: scan", zap.Error(err))
		}
	}
}

// NoticeIdleResearchers tells every researcher due an idle notice that it is idle (agent.idle),
// every idleAfter while its stretch lasts, with what the platform knows at the notice. A platform
// experiment whose numbers cannot be read now is skipped, and its researchers are told at a
// later pass.
func (s *Service) NoticeIdleResearchers(ctx context.Context) error {
	due, err := s.idle.IdleResearchers(ctx, s.idleAfter)
	if err != nil {
		return err
	}
	byRun := map[string][]db.IdleResearcher{}
	for _, r := range due {
		byRun[r.PlatformExperimentID] = append(byRun[r.PlatformExperimentID], r)
	}
	for _, id := range slices.Sorted(maps.Keys(byRun)) {
		if err := s.noticeIdle(ctx, id, byRun[id]); err != nil {
			s.logger.Error("notice_idle_researchers: failed", zap.String("platform_experiment_id", id), zap.Error(err))
		}
	}
	return nil
}

// noticeIdle tells each researcher of due, of platform experiment id, that it is idle.
func (s *Service) noticeIdle(ctx context.Context, id string, due []db.IdleResearcher) error {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return err
	}
	if pe == nil {
		return ErrPlatformExperimentNotFound
	}
	signups, err := s.store.ListAgentSignups(ctx, id)
	if err != nil {
		return err
	}
	rows, err := s.roster(ctx, id, signups, coordinatorReader)
	if err != nil {
		return err
	}
	capacity, err := s.runCapacity(ctx, pe)
	if err != nil {
		return err
	}
	quotas, err := s.ListQuotas(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range due {
		notice := idleNotice{minutes: int(now.Sub(r.Since).Minutes()), fitsNow: capacity.FitsNow, minutesLeft: minutesUntil(pe.EndsAt, now)}
		for _, row := range rows {
			if row.AgentID == r.AgentID {
				notice.subagents, notice.turn = row.Subagents, row.Turn
			}
		}
		for _, q := range quotas {
			if q.AgentID == r.AgentID {
				notice.quota = q
			}
		}
		if err := s.idle.NoticeIdle(ctx, r, notice.detail()); err != nil {
			s.logger.Error("notice_idle_researchers: notice", zap.String("platform_experiment_id", id), zap.String("agent_id", r.AgentID), zap.Error(err))
		}
	}
	return nil
}

// idleNotice is what the platform knows of an idle researcher at its notice.
type idleNotice struct {
	minutes     int
	subagents   *int
	turn        *domain.AgentTurn
	fitsNow     *int
	minutesLeft *int
	quota       *AgentQuotaView
}

// detail is the notice as agent.idle carries it.
func (n idleNotice) detail() string {
	parts := []string{fmt.Sprintf("no job of its own ran on the platform for %d minutes, and it has none queued or running", n.minutes)}
	switch {
	case n.subagents != nil && n.turn != nil:
		parts = append(parts, fmt.Sprintf("its harness reports %d subagents running and its turn %s since %s", *n.subagents, n.turn.State, n.turn.Since.UTC().Format(time.RFC3339)))
	default:
		parts = append(parts, "its harness reports no state")
	}
	if n.fitsNow != nil {
		parts = append(parts, fmt.Sprintf("the free capacity places %d jobs shaped like the baseline job now", *n.fitsNow))
	}
	if n.minutesLeft != nil {
		parts = append(parts, fmt.Sprintf("%d minutes of the run are left", *n.minutesLeft))
	}
	if n.quota != nil {
		left := domain.RoundHours(max(0, n.quota.ShareAcceleratorHours-n.quota.UsedGuaranteedAccH-n.quota.UsedBurstAccH))
		parts = append(parts, fmt.Sprintf("%g of its share of %g accelerator hours are left", left, n.quota.ShareAcceleratorHours))
	}
	return strings.Join(parts, "; ")
}

// NoticeIdleCapacity tells whoever steers each running platform experiment with time left that
// its capacity holds places beyond its queued jobs (capacity.idle), every idleAfter while that
// lasts, whoever has jobs in flight and wherever the best stands against the target. A run whose
// numbers cannot be read is skipped, and told at a later pass.
func (s *Service) NoticeIdleCapacity(ctx context.Context) error {
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpRunning)
	if err != nil {
		return fmt.Errorf("list running platform experiments: %w", err)
	}
	for _, pe := range pes {
		if err := s.noticeIdleCapacity(ctx, pe); err != nil {
			s.logger.Error("notice_idle_capacity: failed", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
		}
	}
	return nil
}

// noticeIdleCapacity records whether the capacity of pe holds places beyond its queued jobs, and
// tells it once that is due, with the numbers the situation reads.
func (s *Service) noticeIdleCapacity(ctx context.Context, pe *domain.PlatformExperiment) error {
	now := time.Now()
	notice := capacityNotice{minutesLeft: minutesUntil(pe.EndsAt, now)}
	if notice.minutesLeft != nil && *notice.minutesLeft > 0 {
		capacity, err := s.runCapacity(ctx, pe)
		if err != nil {
			return err
		}
		signups, err := s.store.ListAgentSignups(ctx, pe.ID)
		if err != nil {
			return err
		}
		rows, err := s.roster(ctx, pe.ID, signups, coordinatorReader)
		if err != nil {
			return err
		}
		notice.jobs = runJobsInFlight(rows)
		if capacity.FitsNow != nil {
			notice.free = *capacity.FitsNow - notice.jobs.Queued
		}
	}
	due, err := s.idle.MarkCapacityFree(ctx, pe.ID, notice.free > 0, s.idleAfter)
	if err != nil || due == nil {
		return err
	}
	objective, err := s.objective(ctx, pe, now)
	if err != nil {
		return err
	}
	if len(objective) > 0 {
		notice.primary = &objective[0]
	}
	notice.minutes = int(now.Sub(due.Since).Minutes())
	return s.idle.NoticeCapacityIdle(ctx, *due, notice.detail())
}

// capacityNotice is what the platform knows of a run whose capacity holds places free.
type capacityNotice struct {
	free        int
	minutes     int
	jobs        JobCounts
	minutesLeft *int
	primary     *SituationMetric
}

// detail is the notice as capacity.idle carries it. A target the best passed is said as passed,
// and the notice stands: the run goes on raising the best.
func (n capacityNotice) detail() string {
	parts := []string{fmt.Sprintf("the free capacity places %d more jobs shaped like the baseline job beyond the %d queued, and has for %d minutes", n.free, n.jobs.Queued, n.minutes),
		fmt.Sprintf("%d jobs of the run are queued and %d running", n.jobs.Queued, n.jobs.Running)}
	if n.minutesLeft != nil {
		parts = append(parts, fmt.Sprintf("%d minutes of the run are left", *n.minutesLeft))
	}
	switch m := n.primary; {
	case m == nil || m.BestValue == nil:
		parts = append(parts, "no best is measured yet")
	case m.GapToTargetPct == nil || m.Target == nil:
		parts = append(parts, fmt.Sprintf("the best %s is %g, against no target", m.Metric, *m.BestValue))
	case *m.GapToTargetPct > 0:
		parts = append(parts, fmt.Sprintf("the best %s is %g, %g%% short of the target %g", m.Metric, *m.BestValue, *m.GapToTargetPct, *m.Target))
	default:
		parts = append(parts, fmt.Sprintf("the best %s is %g, %g%% past the target %g", m.Metric, *m.BestValue, -*m.GapToTargetPct, *m.Target))
	}
	return strings.Join(parts, "; ")
}

// StartEndSweep runs SweepEnded on a ticker until ctx is cancelled: StartExpirySweep's interval
// would close a run up to that long after its end. Each pass is bounded by passTimeout.
func (s *Service) StartEndSweep(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, passTimeout)
			if err := s.SweepEnded(passCtx); err != nil {
				s.logger.Error("sweep_ended: scan", zap.Error(err))
			}
			cancel()
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
