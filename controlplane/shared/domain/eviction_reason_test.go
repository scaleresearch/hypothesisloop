package domain

import (
	"strings"
	"testing"
)

// The two silence reasons must stay distinct: an operator seeing "silent" looks for a hung
// training process, which is the wrong hunt when the reporting path never worked at all.
func TestSilenceReasonsAreDistinct(t *testing.T) {
	if EvictionSilent == EvictionNeverReportedMetrics {
		t.Fatal("silence reasons collapsed to one value")
	}
	for name, got := range map[string]EvictionReason{
		"silent":                 EvictionSilent,
		"never_reported_metrics": EvictionNeverReportedMetrics,
	} {
		if string(got) != name {
			t.Errorf("%s wire value = %q, want %q", name, got, name)
		}
	}
}

// A failed job is announced with how its process ended and with nothing else the runtime last
// said of it: a job that failed while its last report was about starting has no cause to give.
func TestFailureCauseIsHowTheProcessEnded(t *testing.T) {
	for _, c := range []struct {
		detail PhaseDetail
		want   string
	}{
		{PhaseDetail{Reason: PhaseReasonContainerFailed, Message: "command exited with code 1"}, "container_failed: command exited with code 1"},
		{PhaseDetail{Reason: PhaseReasonOOMKilled, Message: "container exited with code 137 (OOMKilled): killed\nat step 3"}, "oom_killed: container exited with code 137 (OOMKilled): killed"},
		{PhaseDetail{Reason: PhaseReasonContainerFailed}, "container_failed"},
		{PhaseDetail{Reason: PhaseReasonInstanceStarting, Message: "a machine is rented"}, ""},
		{PhaseDetail{}, ""},
	} {
		if got := c.detail.FailureCause(); got != c.want {
			t.Errorf("FailureCause(%+v) = %q, want %q", c.detail, got, c.want)
		}
	}
	long := PhaseDetail{Reason: PhaseReasonContainerFailed, Message: strings.Repeat("é", 500)}
	if got := long.FailureCause(); got != "container_failed: "+strings.Repeat("é", failureCauseDetailRunes) {
		t.Errorf("a long message was not cut to %d characters: %d bytes", failureCauseDetailRunes, len(got))
	}
}
