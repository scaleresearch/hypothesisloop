// Package dedup provides hypothesis novelty scoring for the HypothesisLoop scheduler.
//
// Advisory only — feeds priority scoring, never a hard rejection. Real duplicate-hypothesis
// rejection lives in services/registry via a DB-level UNIQUE index on normalized text.
//
// Novelty here counts how many active (RUNNING/QUEUED/SUBMITTED) experiments already reference
// the same hypothesis_id — more existing = less novel.
//
// Upgrade path: swap the counting below for TF-IDF/embedding similarity over hypotheses.text
// if free-text similarity across distinct hypotheses is ever wanted. Interface stays the same.
package dedup

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Service implements scheduler.NoveltyDetector.
// It returns a novelty score in [0, 1] where 1.0 = no other active experiment shares this hypothesis.
type Service struct{}

// New returns a dedup Service.
func New() *Service { return &Service{} }

// ComputeNovelty returns 1 / (1 + n), where n is the count of existingExperiments referencing
// the same hypothesisID. An empty hypothesisID always returns 1.0 (callers must validate it's set).
func (s *Service) ComputeNovelty(_ context.Context, hypothesisID string, existingExperiments []*domain.Experiment) (float64, error) {
	if hypothesisID == "" {
		return 1.0, nil
	}
	var n float64
	for _, exp := range existingExperiments {
		if exp.HypothesisID == hypothesisID {
			n++
		}
	}
	return 1.0 / (1.0 + n), nil
}
