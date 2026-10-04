package domain

import "testing"

func TestOnlyAFaultOfTheClustersOwnPutsItOnTheTriedList(t *testing.T) {
	for reason, class := range faultClasses {
		want := class == FaultInfrastructure && reason != EvictionCodeUnreachable && reason != EvictionNoOffer && reason != EvictionEnvironmentUnbuilt
		if got := FailedByItsCluster(reason.WithDetail("a sentence")); got != want {
			t.Errorf("%s: got %v, want %v", reason, got, want)
		}
	}
}

// An environment the platform did not build is refunded like every infrastructure fault, and its
// job is not queued again: it would end again on the next tick.
func TestAnUnbuiltEnvironmentIsRefundedAndNotRequeued(t *testing.T) {
	for reason, class := range faultClasses {
		want := class == FaultInfrastructure && reason != EvictionEnvironmentUnbuilt
		if got := RequeuedOnFault(reason.WithDetail("a sentence")); got != want {
			t.Errorf("%s: requeued %v, want %v", reason, got, want)
		}
	}
	if !IsInfrastructureFault(EvictionEnvironmentUnbuilt.WithDetail("a sentence")) {
		t.Error("environment_unbuilt is not an infrastructure fault, so it would not be refunded")
	}
}
