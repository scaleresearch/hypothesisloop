package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A hypothesis write and a close of its platform experiment are ordered by the experiment's row:
// a close waits for a write that passed the check, and every write after the close is refused.
func TestAHypothesisWriteHoldsOffACloseAndNoneIsTakenAfterIt(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pe := "closedwrites-pe-" + uuid.New().String()[:8]
	hypothesis := "closedwrites-hyp-" + uuid.New().String()[:8]
	createTestPE(t, pool, pe, 10)
	events := NewEventsStore(pool, noHypothesisEvents{})
	hypotheses := NewStore(pool, 3)
	person := domain.Actor{Kind: domain.ActorUser, ID: "user-ada"}

	if _, err := hypotheses.CreateHypothesisComment(ctx, hypothesis, pe, "", person, "while open"); err != nil {
		t.Fatalf("comment while open: got = %v, want nil", err)
	}

	passed, closed := make(chan struct{}), make(chan error, 1)
	go func() {
		<-passed
		closed <- NewStore(pool, 3).ClosePlatformExperiment(ctx, pe, &person, "")
	}()
	err := events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) {
		if err := stamp.RequireOpen(pe); err != nil {
			return nil, err
		}
		close(passed)
		select {
		case err := <-closed:
			t.Errorf("the close committed while a write that passed was still in flight: %v", err)
			closed <- err
		case <-time.After(300 * time.Millisecond):
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("a write while open: got = %v, want nil", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("close: got = %v, want nil", err)
	}

	err = events.Emit(ctx, func(stamp domain.Stamp) ([]domain.Event, error) { return nil, stamp.RequireOpen(pe) })
	if !errors.Is(err, domain.ErrPlatformExperimentClosed) {
		t.Errorf("a graph write after the close: got = %v, want %v", err, domain.ErrPlatformExperimentClosed)
	}
	if _, err := hypotheses.CreateHypothesisComment(ctx, hypothesis, pe, "", person, "after the close"); !errors.Is(err, domain.ErrPlatformExperimentClosed) {
		t.Errorf("a comment after the close: got = %v, want %v", err, domain.ErrPlatformExperimentClosed)
	}
	if count, err := hypotheses.CountCommentsByHypothesis(ctx, hypothesis); err != nil || count != 1 {
		t.Errorf("comments kept: got = (%d, %v), want only the one made while open", count, err)
	}
}
