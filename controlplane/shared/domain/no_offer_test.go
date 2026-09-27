package domain

import "testing"

func TestAPlacementThatFoundNoMachineIsEndedAsNoOfferAndIsTheEnvironmentsFault(t *testing.T) {
	said := "no machine to rent for shape rtx4090 within the provider's configured filters in 3m0s of asking"
	eviction := PhaseDetail{Reason: PhaseReasonNoOffer, Message: said}.EvictionOfNeverStarting()
	if eviction != EvictionReason("no_offer: "+said) || !IsInfrastructureFault(eviction) {
		t.Fatalf("got %q, infrastructure %v", eviction, IsInfrastructureFault(eviction))
	}
	if eviction := (PhaseDetail{Reason: PhaseReasonNoOffer}).EvictionOfNeverStarting(); eviction != EvictionNoOffer {
		t.Fatalf("got %q", eviction)
	}
	for _, reason := range []string{PhaseReasonImagePullFailed, PhaseReasonConfigError, PhaseReasonInvalidImage} {
		eviction := PhaseDetail{Reason: reason, Message: "manifest unknown"}.EvictionOfNeverStarting()
		if eviction != EvictionReason("unschedulable: "+reason+": manifest unknown") || eviction.Code() != EvictionUnschedulable || IsInfrastructureFault(eviction) {
			t.Fatalf("%s is ended as %q", reason, eviction)
		}
		if eviction := (PhaseDetail{Reason: reason}).EvictionOfNeverStarting(); eviction != EvictionReason("unschedulable: "+reason) {
			t.Fatalf("%s with no message is ended as %q", reason, eviction)
		}
	}
}
