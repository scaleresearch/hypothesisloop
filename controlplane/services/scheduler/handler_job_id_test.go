package scheduler

import (
	"regexp"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// hl made each job's id, twelve hex digits of its own, and a submit that stated none was refused.
// The platform issues it, in the one shape of every id it issues.
func TestAJobThatStatesNoIDIsIssuedOneByThePlatform(t *testing.T) {
	issued := regexp.MustCompile(`^job-[0-9a-f]{32}$`)
	first, err := newJob(submitRequest{}, submitMeta{}, domain.Actor{})
	if err != nil || !issued.MatchString(first.ID) || !experimentIDPattern.MatchString(first.ID) {
		t.Fatalf("got id %q, %v, want job- and 32 hex digits, usable in a cluster's names", first.ID, err)
	}
	if second, _ := newJob(submitRequest{}, submitMeta{}, domain.Actor{}); second.ID == first.ID {
		t.Fatalf("two jobs were issued one id: %s", first.ID)
	}
}
