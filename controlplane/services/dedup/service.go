// Package dedup provides hypothesis novelty scoring for the OpenResearch scheduler.
//
// This is advisory only — it feeds priority scoring, never a hard rejection. Real
// duplicate-hypothesis *rejection* lives in services/registry: hypotheses are registered
// through a DB-level UNIQUE index on normalized text, so two agents naming the same claim
// share one hypothesis_id instead of creating two rows. That's the actual uniqueness check.
//
// Given that, novelty here reduces to counting how many active (RUNNING/QUEUED/SUBMITTED)
// experiments already reference the same hypothesis_id as the one being scored — the more
// that already exist, the less novel another submission testing it is.
//
// Planned upgrade path (unchanged from before): once free-text similarity across distinct
// hypotheses is wanted (not just exact hypothesis_id matches), replace the counting below
// with TF-IDF/embedding similarity over hypotheses.text. The interface (scheduler.NoveltyDetector)
// stays the same either way.
package dedup

import (
	"context"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Service implements scheduler.NoveltyDetector.
// It returns a novelty score in [0, 1] where 1.0 = no other active experiment shares this hypothesis.
type Service struct{}

// New returns a dedup Service.
func New() *Service { return &Service{} }

// ComputeNovelty returns 1 / (1 + n), where n is the number of experiments in
// existingExperiments that reference the same hypothesisID. An empty hypothesisID never
// matches (novelty is undefined for it; callers must have already validated it's set).
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
