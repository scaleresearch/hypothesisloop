package scheduler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

var errAdmissionCapacityChanged = errors.New("capacity changed during admission")

// concurrencyCapExceeded is implemented by the quota package's concurrencyCapError; checked via
// errors.As so submitJob's callers can distinguish autoscaler.md's max_concurrent_accelerators
// rejection from a generic submit failure without importing the quota package's concrete type.
type concurrencyCapExceeded interface{ ConcurrencyCapExceeded() bool }

// readAcceleratorTypeCapacity is GetAcceleratorTypeCapacity with the clusters it left out logged and the read kept.
func readAcceleratorTypeCapacity(ctx context.Context, client interface {
	GetAcceleratorTypeCapacity(ctx context.Context) (available, desiredFree map[string]domain.Footprint, err error)
}, logger *zap.Logger) (available, desiredFree map[string]domain.Footprint, err error) {
	available, desiredFree, err = client.GetAcceleratorTypeCapacity(ctx)
	var skipped InconsistentSnapshots
	if errors.As(err, &skipped) {
		for cluster, why := range skipped {
			logger.Warn("cluster left out of the capacity read", zap.String("cluster", cluster), zap.Error(why))
		}
		return available, desiredFree, nil
	}
	return available, desiredFree, err
}

type insufficientQuota interface {
	error
	InsufficientQuota() bool
}

// notAdmittedReasonForSubmitError maps a submitJob/submitJobTo error to the not_admitted_reason
// the row should carry. Order matters: check the most specific error first.
func notAdmittedReasonForSubmitError(err error) string {
	var capErr concurrencyCapExceeded
	var quotaErr insufficientQuota
	var limitErr computeLimitReached
	switch {
	case errors.As(err, &capErr):
		return concurrencyCapReason()
	case errors.As(err, &quotaErr):
		return domain.NotAdmittedInsufficientQuota + ": " + quotaErr.Error()
	case errors.Is(err, errAdmissionCapacityChanged):
		return capacityTakenReason()
	case errors.As(err, &limitErr):
		return limitErr.reason()
	default:
		return workloadCreationReason()
	}
}

// computeLimitReached is a claim on a shared cluster that would take the job's org or project past
// its platform compute limit.
type computeLimitReached struct{ *domain.ClaimRefusal }

func (e computeLimitReached) Error() string { return e.reason() }

func (e computeLimitReached) reason() string {
	holder := "org"
	if e.Code == domain.NotAdmittedProjectComputeLimit {
		holder = "project"
	}
	return fmt.Sprintf("%s: %s %s holds %.1f of %.1f AccH/h of platform compute; the job needs %.1f. It is admitted as the %s's jobs on platform clusters end",
		e.Code, holder, e.Holder, e.Held, e.Limit, e.Needs, holder)
}

// submitErrorLevel logs a lost race for the last free capacity, a quota refusal and a platform
// compute limit at info: the job stays queued with the reason on its row, so none is an error.
func submitErrorLevel(err error) zapcore.Level {
	var quotaErr insufficientQuota
	var limitErr computeLimitReached
	if errors.Is(err, errAdmissionCapacityChanged) || errors.As(err, &quotaErr) || errors.As(err, &limitErr) {
		return zapcore.InfoLevel
	}
	return zapcore.ErrorLevel
}

// persistedAcceleratorType is what the experiments row said before this tick's placement search ran.
// Passing it in is what makes the write below correct: the search communicates its choice by
// mutating exp.AcceleratorType, so by the time submitJob runs, exp no longer knows what is
// actually stored. Comparing the choice against the immutable *requested* accelerator type instead used to
// leave the row stranded on a substitute — tick 1 persists accelerator type B and then fails its claim,
// tick 2 re-picks the originally requested A, the requested-type comparison sees no
// substitution and writes nothing, and the row says B for the rest of the job's life while the
// scheduler reserves and fit-checks A.
// speculative marks a submit made against a cluster with no live capacity — the SUBMITTED row
// itself is the scale-up request, so there is no fresh node to re-check a live-fit predicate
// against at claim time. See ClaimSubmitted: a nil capacityAvailable callback skips that check
// and relies on the lock plus the status='QUEUED' predicate alone.
func (l *Loop) submitJob(ctx context.Context, exp *domain.Experiment, clusterName string, persistedAcceleratorType domain.AcceleratorType) error {
	return l.submitJobTo(ctx, exp, clusterName, "", persistedAcceleratorType, false, false)
}

// provenFit mirrors speculativeCandidates' own signal for this cluster (false for a non-speculative
// submit, where it is unused) — the claim-time recheck below must apply the exact same default cap
// speculativeCandidates applied a moment ago, or a cluster with no live proof and no operator cap
// stays unbounded at the one place multiple concurrent scheduler replicas actually serialize
// against each other: the tick-local snapshot alone cannot close this.
func (l *Loop) submitJobTo(ctx context.Context, exp *domain.Experiment, clusterName, clusterID string, persistedAcceleratorType domain.AcceleratorType, speculative, provenFit bool) error {
	// ReserveAdmittedAcceleratorType is called on every submit now, not only on an accelerator type substitution: it
	// is also where the max_concurrent_accelerators cap (autoscaler.md's concurrency cap) is
	// enforced atomically against this pool's live SUBMITTED+RUNNING accelerator count, and
	// speculation makes live capacity appear on demand — the cap is the only thing left bounding
	// concurrency once that gate is open. Every job holds units of a priced type (ValidateExperiment),
	// cores on CPU and devices otherwise, so there is none it does not apply to.
	newEstCost := exp.EstimatedCostAccH
	acceleratorType := persistedAcceleratorType
	if exp.AcceleratorType != persistedAcceleratorType {
		held, err := exp.EstimateHeld()
		if err != nil {
			return fmt.Errorf("submitJob: %w", err)
		}
		newEstCost = held
		acceleratorType = exp.AcceleratorType
	}
	if err := l.quota.ReserveAdmittedAcceleratorType(ctx, exp.ID, acceleratorType, newEstCost, exp.AcceleratorCount); err != nil {
		return fmt.Errorf("reserve selected accelerator type: %w", err)
	}
	exp.EstimatedCostAccH = newEstCost
	fp := exp.Footprint()
	var capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)
	if speculative {
		// There is no live node to re-check a fit predicate against (that's why this is
		// speculative), but ClaimSubmitted's per-cluster advisory lock and its `desired` read
		// (every SUBMITTED row on clusterName) are still the one place a fresh,
		// serialized-across-replicas view of this cluster's speculative footprint exists —
		// reuse them to recheck max_speculative_accelerators here rather than trusting the
		// tick-local snapshot speculativeCandidates saw a moment ago (a snapshot other replicas
		// could have raced past concurrently).
		capacityAvailable = func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
			if clusterID == "" {
				return true, nil
			}
			cap, err := l.clusterSpeculativeCap(ctx, clusterID)
			if err != nil {
				return false, err
			}
			if cap == nil && !provenFit {
				// Same default speculativeCandidates applied at selection time: no live proof and
				// no operator override means bound the exposure to this one job's own footprint,
				// not leave it unbounded — otherwise many concurrent replicas, each seeing a
				// tick-local snapshot with zero footprint, could each independently land their own
				// "one job's worth" on the same unproven cluster before any single failure ever
				// triggers tried_clusters backoff.
				oneJob := exp.AcceleratorCount
				cap = &oneJob
			}
			if cap == nil {
				return true, nil
			}
			// desired is every SUBMITTED row on this cluster, not only the speculative
			// ones — the doc's "None: derived from autoscaler_enabled ∧ scheduled_nodes <
			// Nodes()" decision means whether a given row is still riding on a scale-up or has
			// since landed on live capacity is metrics-store state, unavailable inside this
			// Postgres transaction. Counting every SUBMITTED row here is deliberately
			// conservative: it can delay a legitimate speculative submit behind a cluster's
			// live-fit traffic (retried next tick, nothing lost), which is the safe direction to
			// err in for an optional operator-set cap — the opposite (undercounting) is the
			// unbounded-overshoot failure mode this recheck exists to close.
			footprint := exp.AcceleratorCount
			for _, d := range desired {
				footprint += d.AcceleratorCount
			}
			return footprint <= *cap, nil
		}
	} else {
		capacityAvailable = func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
			available, _, err := readAcceleratorTypeCapacity(ctx, l.clusters, l.logger)
			if err != nil {
				return false, err
			}
			if !domain.Fits(available[clusterName], fp) {
				return false, nil
			}
			nodeAvail, err := l.clusters.GetAcceleratorCapacityByNode(ctx)
			if err != nil {
				return false, err
			}
			nodeResources, err := l.clusters.GetNodeResourceCapacity(ctx)
			if err != nil {
				return false, err
			}
			nodeLabels, err := l.clusters.GetNodeLabels(ctx)
			if err != nil {
				return false, err
			}
			return placement.DesiredFits(nodeAvail[clusterName], nodeResources[clusterName], nodeLabels[clusterName], desired, exp, exp.ResolvedJob), nil
		}
	}
	resolved, err := l.grace.Resolve(exp.EffectiveJob())
	if err != nil {
		return err
	}
	if exp.ImageArchitecture != "" {
		selector := maps.Clone(resolved.NodeSelector)
		if selector == nil {
			selector = map[string]string{}
		}
		selector[domain.NodeArchitectureLabel] = exp.ImageArchitecture
		resolved.NodeSelector = selector
	}
	exp.ResolvedJob = &resolved
	if err := l.observed.RecordAdmission(ctx, exp.ID, time.Now().UTC()); err != nil {
		return err
	}
	refused, err := l.store.ClaimSubmitted(ctx, exp.ID, clusterName, exp.ResolvedJob, capacityAvailable)
	if err != nil {
		return err
	}
	switch {
	case refused == nil:
	case refused.Code == domain.NotAdmittedCapacityUnavailable:
		return errAdmissionCapacityChanged
	default:
		return computeLimitReached{refused}
	}
	exp.ClusterName = clusterName
	return nil
}

// quotaKey builds the (AgentID, PlatformExperimentID) composite key quota is tracked under —
// matches fetchQuotaMap's dedup key, so every consumer agrees on the lookup.
func quotaKey(agentID, platformExpID string) string {
	return agentID + "/" + platformExpID
}

// fetchQuotaMap builds a map of (agentID, platformExperimentID) -> *domain.AgentQuota for
// guaranteed/burst ordering. Keyed by the composite pair, not AgentID alone, since an agent can
// run multiple platform experiments concurrently each with its own quota pool. The full
// AgentQuota (not a precomputed ratio) is kept so sortGuaranteed/sortBurst can compute each
// experiment's own dominant-utilization ratio against the dimensions that job actually requests.
//
// An agent whose quota cannot be read has its jobs skipped this tick; the returned slice is exps
// without them.
func (l *Loop) fetchQuotaMap(ctx context.Context, exps []*domain.Experiment, tickErrs *[]error) ([]*domain.Experiment, map[string]*domain.AgentQuota) {
	result := map[string]*domain.AgentQuota{}
	failed := map[string]error{}
	kept := exps[:0]
	for _, exp := range exps {
		if exp.PlatformExperimentID == "" {
			kept = append(kept, exp)
			continue
		}
		key := quotaKey(exp.AgentID, exp.PlatformExperimentID)
		if result[key] == nil && failed[key] == nil {
			aq, err := l.quota.GetAgentQuota(ctx, exp.AgentID, exp.PlatformExperimentID)
			if err == nil && aq == nil {
				err = errors.New("quota not found")
			}
			if err != nil {
				failed[key] = fmt.Errorf("fetch quota for %s: %w", key, err)
			} else {
				result[key] = aq
			}
		}
		if err := failed[key]; err != nil {
			l.skipExperiment(tickErrs, exp, err)
			continue
		}
		kept = append(kept, exp)
	}
	return kept, result
}
