package auth

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"go.uber.org/zap"
)

type storeDown struct{}

func (storeDown) Error() string { return "dial tcp: timeout" }
func (storeDown) Unavailable() (string, string) {
	return "object_store_unavailable", "the object store is not answering"
}

func TestADependencyThatWasDownIsAnswered503WithItsReason(t *testing.T) {
	refusal := Internal(zap.NewNop(), "submit experiment", fmt.Errorf("scheduler: measure agent durable data: %w", storeDown{}))
	if refusal.GetStatus() != http.StatusServiceUnavailable || refusal.Reason != "object_store_unavailable" || refusal.Message != "the object store is not answering" {
		t.Fatalf("got %d %q %q", refusal.GetStatus(), refusal.Reason, refusal.Message)
	}
}

func TestAnyOtherFailureIsABare500(t *testing.T) {
	refusal := Internal(zap.NewNop(), "submit experiment", errors.New("pq: relation does not exist"))
	if refusal.GetStatus() != http.StatusInternalServerError || refusal.Reason != "" || refusal.Message != InternalError {
		t.Fatalf("got %d %q %q", refusal.GetStatus(), refusal.Reason, refusal.Message)
	}
}
