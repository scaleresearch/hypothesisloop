package domain

import "testing"

func TestOnlyAFaultOfTheClustersOwnPutsItOnTheTriedList(t *testing.T) {
	for reason, class := range faultClasses {
		want := class == FaultInfrastructure && reason != EvictionCodeUnreachable && reason != EvictionNoOffer
		if got := FailedByItsCluster(reason.WithDetail("a sentence")); got != want {
			t.Errorf("%s: got %v, want %v", reason, got, want)
		}
	}
}
