package db

import (
	"context"
	"testing"
)

// Every SUBMITTED row carries the job as the runtime must run it; a claim without one would hand
// a runtime a job with no resolved grace.
func TestClaimSubmittedRequiresAResolvedJob(t *testing.T) {
	if _, err := (&ExperimentsStore{}).ClaimSubmitted(context.Background(), "exp-1", "cluster-a", nil, nil); err == nil {
		t.Fatal("ClaimSubmitted accepted a nil resolved job")
	}
}
