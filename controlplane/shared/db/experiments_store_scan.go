package db

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ---- helpers ----

type rowScanner interface {
	Scan(dest ...any) error
}

func scanExperiment(row rowScanner) (*domain.Experiment, error) {
	exp := &domain.Experiment{}
	var acceleratorType, capacityTier, status string
	var evictionReason, notAdmittedReason *string
	var jobSpec []byte

	if err := row.Scan(
		&exp.ID, &exp.ParentID, &exp.AgentID, &exp.PlatformExperimentID, &exp.ProjectID, &exp.ClusterName,
		&exp.CodeRef, &exp.ConfigHash, &exp.DataRef, &jobSpec,
		&exp.HypothesisID, &exp.Hypothesis, &exp.Objective, &exp.Theory,
		&acceleratorType, &exp.AcceleratorCount,
		&exp.EstimatedDurationHours, &exp.EstimatedCostAccH,
		&exp.EstimatedCPUCoreHours, &exp.EstimatedRAMGBHours, &exp.EstimatedStorageGBHours,
		&exp.PriorityScore, &exp.NoveltyScore, &capacityTier, &status,
		&exp.QueuedAt, &exp.SubmittedAt, &evictionReason, &notAdmittedReason,
		&exp.QuotaSettledAt, &exp.AttemptCount,
		&exp.CreatedAt, &exp.UpdatedAt,
	); err != nil {
		return nil, err
	}

	if len(jobSpec) > 0 {
		if err := json.Unmarshal(jobSpec, &exp.Job); err != nil {
			return nil, fmt.Errorf("scan experiment: unmarshal job spec: %w", err)
		}
	}
	exp.AcceleratorType = domain.AcceleratorType(acceleratorType)
	nodes := exp.Job.Nodes()
	if nodes <= 0 || exp.AcceleratorCount%nodes != 0 {
		return nil, fmt.Errorf("scan experiment %s: total accelerator_count %d is not divisible by num_nodes %d", exp.ID, exp.AcceleratorCount, nodes)
	}
	exp.Job.AcceleratorCount = exp.AcceleratorCount / nodes
	exp.CapacityTier = domain.CapacityTier(capacityTier)
	exp.Status = domain.ExperimentStatus(status)
	// exp.Job.AcceleratorType stays the type the agent originally asked for while
	// exp.AcceleratorType tracks the flavor admission actually chose. Do not collapse the two
	// here: the scheduler compares them to undo a substitution whose flavor quota can no longer
	// cover (loop_tick.go, loop_preempt.go), and the pod is already built from the concrete
	// exp.AcceleratorType (k8sexec.job_build), so nothing downstream needs the JobSpec rewritten.
	if evictionReason != nil {
		exp.EvictionReason = *evictionReason
	}
	if notAdmittedReason != nil {
		exp.NotAdmittedReason = *notAdmittedReason
	}
	return exp, nil
}

func collectExperiments(rows pgx.Rows) ([]*domain.Experiment, error) {
	// Non-nil even when empty: a nil slice serializes to JSON `null` instead of `[]`, which
	// breaks any client-side code that assumes a list endpoint always returns an array (see
	// hypotheses_store.ListHypotheses for the same fix and the bug it caused on the
	// hypotheses UI page).
	out := []*domain.Experiment{}
	for rows.Next() {
		exp, err := scanExperiment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan experiment: %w", err)
		}
		out = append(out, exp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
