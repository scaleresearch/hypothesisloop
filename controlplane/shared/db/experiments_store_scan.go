package db

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// ---- helpers ----

type rowScanner interface {
	Scan(dest ...any) error
}

func scanExperiment(row rowScanner) (*domain.Experiment, error) {
	exp := &domain.Experiment{}
	var acceleratorType, capacityTier, status string
	var artifacts []string
	var evictionReason *string
	var notAdmittedReason *string
	var jobSpec []byte

	if err := row.Scan(
		&exp.ID, &exp.ParentID, &exp.AgentID, &exp.PlatformExperimentID, &exp.ProjectID, &exp.ClusterName,
		&exp.CodeRef, &exp.ConfigHash, &exp.DataRef, &jobSpec,
		&exp.HypothesisID, &exp.Hypothesis, &exp.Objective, &exp.Theory,
		&acceleratorType, &exp.AcceleratorCount,
		&exp.EstimatedDurationHours, &exp.EstimatedCostAccH,
		&exp.EstimatedCPUCoreHours, &exp.EstimatedRAMGBHours, &exp.EstimatedStorageGBHours,
		&exp.PriorityScore, &exp.NoveltyScore, &capacityTier, &status,
		&exp.QueuedAt, &exp.SubmittedAt, &exp.StartedAt, &exp.PreemptCount, &exp.Attempt, &evictionReason, &notAdmittedReason,
		&exp.ActualDurationHours, &exp.ActualCostAccH,
		&exp.ActualCPUCoreHours, &exp.ActualRAMGBHours, &exp.ActualStorageGBHours,
		&artifacts, &exp.QuotaSettledAt,
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
	exp.CapacityTier = domain.CapacityTier(capacityTier)
	exp.Status = domain.ExperimentStatus(status)
	if evictionReason != nil {
		exp.EvictionReason = *evictionReason
	}
	if notAdmittedReason != nil {
		exp.NotAdmittedReason = *notAdmittedReason
	}
	if artifacts == nil {
		artifacts = []string{}
	}
	exp.Artifacts = artifacts

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
