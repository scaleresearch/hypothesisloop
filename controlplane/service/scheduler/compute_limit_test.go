package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A claim a platform compute limit refused is queued with the holder and the figures, and waits.
func TestAComputeLimitRefusalIsQueuedWithItsFigures(t *testing.T) {
	err := computeLimitReached{&domain.ClaimRefusal{Code: domain.NotAdmittedProjectComputeLimit, Holder: "P", Held: 3, Limit: 4, Needs: 1.5}}
	reason := notAdmittedReasonForSubmitError(err)
	if want := "project_compute_limit: project P holds 3.0 of 4.0 AccH/h of platform compute; the job needs 1.5"; !strings.HasPrefix(reason, want) {
		t.Errorf("got = %q, want it to start %q", reason, want)
	}
	if !domain.QueueReasonClearsByWaiting(reason) || submitErrorLevel(err).String() != "info" {
		t.Errorf("a compute limit is a wait: clears by waiting %v, logged at %s", domain.QueueReasonClearsByWaiting(reason), submitErrorLevel(err))
	}
	org := notAdmittedReasonForSubmitError(computeLimitReached{&domain.ClaimRefusal{Code: domain.NotAdmittedOrgComputeLimit, Holder: "acme", Held: 7, Limit: 8, Needs: 2}})
	if !strings.HasPrefix(org, "org_compute_limit: org acme holds 7.0 of 8.0 AccH/h") {
		t.Errorf("the org's: got = %q", org)
	}
}

// A job that alone reserves more than its project may hold of the platform's compute runs on no
// platform cluster, so it never fits where no cluster of its org's own may hold it.
func TestAJobAboveItsProjectsLimitNeverFitsWithoutAClusterOfItsOrg(t *testing.T) {
	big := holdJob("big", domain.CapacityGuaranteed, h100, 4, "1")
	big.OrgID = "org-a"
	room := domain.CapacityFootprint(64, map[string]int64{h100: 8}, 1<<40, 1<<42)
	platform := domain.Cluster{Name: "platform", OrgID: domain.PlatformOrgID}
	own := domain.Cluster{Name: "own", OrgID: "org-a"}
	limits := func(project float64) map[string]domain.ComputeLimit {
		return map[string]domain.ComputeLimit{"org-a": {Org: 8, Project: project}}
	}
	onlyPlatform := installed{registered: map[string]domain.Cluster{"platform": platform}, totals: map[string]domain.Footprint{"platform": room}, limits: limits(3)}
	reason := onlyPlatform.neverFitsReason(big)
	if code, message := domain.SplitQueueReason(reason); code != domain.NotAdmittedExceedsProjectComputeLimit ||
		!strings.Contains(message, "4.0 AccH/h") || !strings.Contains(message, "3.0") || !strings.Contains(message, "platform is the platform's") {
		t.Fatalf("4 AccH/h under a project limit of 3: got = %q", reason)
	}
	for name, in := range map[string]installed{
		"at the limit":                        {registered: onlyPlatform.registered, totals: onlyPlatform.totals, limits: limits(4)},
		"beside a cluster of its own":         {registered: map[string]domain.Cluster{"platform": platform, "own": own}, totals: map[string]domain.Footprint{"platform": room, "own": room}, limits: limits(3)},
		"beside its own that reports nothing": {registered: map[string]domain.Cluster{"platform": platform, "own": own}, totals: map[string]domain.Footprint{"platform": room}, limits: limits(3)},
	} {
		if got := in.neverFitsReason(big); got != "" {
			t.Errorf("%s: got = %q, want the job to wait", name, got)
		}
	}
	lacking := installed{registered: map[string]domain.Cluster{"platform": platform, "own": own}, totals: map[string]domain.Footprint{"platform": room, "own": domain.CapacityFootprint(64, map[string]int64{h100: 1}, 1<<40, 1<<42)}, limits: limits(3)}
	if code, _ := domain.SplitQueueReason(lacking.neverFitsReason(big)); code != domain.NotAdmittedExceedsClusterCapacity {
		t.Errorf("its own cluster too small and the platform's over the limit: got = %q, want %s", code, domain.NotAdmittedExceedsClusterCapacity)
	}
}

// Submit refuses such a job with 422 exceeds_project_compute_limit.
func TestAJobAboveItsProjectsLimitIsRefusedAtSubmit(t *testing.T) {
	registered := map[string]domain.Cluster{"platform": {Name: "platform", OrgID: domain.PlatformOrgID}}
	capacity := installedCapacity{installed: map[string]domain.Footprint{"platform": domain.CapacityFootprint(64, map[string]int64{h100: 8}, 1<<40, 1<<42)}}
	submit := func(project float64) (*AdmissionError, error) {
		store := clustersStore{registered: registered, limits: map[string]domain.ComputeLimit{"org-a": {Org: 8, Project: project}}}
		s := &Service{credits: testCaps, codeHost: testCodeHost{}, store: store, clusters: capacity, hypotheses: humanHypothesis("pe-1")}
		var refused *AdmissionError
		err := s.Submit(context.Background(), humanIdeaSubmission())
		errors.As(err, &refused)
		return refused, err
	}
	if _, err := submit(1); !errors.Is(err, errReachedScoring) {
		t.Fatalf("1 AccH/h under a project limit of 1: got = %v, want it accepted", err)
	}
	refused, err := submit(0.5)
	if refused == nil || refused.Reason != domain.NotAdmittedExceedsProjectComputeLimit || AdmissionStatus(refused.Reason) != http.StatusUnprocessableEntity {
		t.Fatalf("1 AccH/h under a project limit of 0.5: got = %v, want a 422 %s", err, domain.NotAdmittedExceedsProjectComputeLimit)
	}
}
