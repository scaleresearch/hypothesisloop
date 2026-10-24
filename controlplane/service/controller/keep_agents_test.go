package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// keptNothing keeps no agents: the tests it serves are about something else.
type keptNothing struct{}

func (keptNothing) KeepAgents(context.Context, *domain.PlatformExperiment, time.Time) error {
	return nil
}

// keeperFailingFor fails to keep the teams of the platform experiments it names, and
// records every one it was asked to keep.
type keeperFailingFor struct {
	failing map[string]bool
	asked   []string
}

func (k *keeperFailingFor) KeepAgents(_ context.Context, pe *domain.PlatformExperiment, _ time.Time) error {
	k.asked = append(k.asked, pe.ID)
	if k.failing[pe.ID] {
		return errors.New("store unreachable for " + pe.ID)
	}
	return nil
}

// One platform experiment whose team cannot be kept now leaves the others kept in the same
// pass, and the failure is told.
func TestOneRunWhoseTeamCannotBeKeptLeavesTheOthersKept(t *testing.T) {
	keeper := &keeperFailingFor{failing: map[string]bool{"pe-2": true}}
	c := New(nil, nil, zap.NewNop()).WithAgentKeeper(keeper)
	pes := []*domain.PlatformExperiment{{ID: "pe-1"}, {ID: "pe-2"}, {ID: "pe-3"}}

	errs := c.keepAgents(context.Background(), pes, time.Now())
	if !slices.Equal(keeper.asked, []string{"pe-1", "pe-2", "pe-3"}) {
		t.Errorf("asked to keep %v, want every running platform experiment", keeper.asked)
	}
	if len(errs) != 1 {
		t.Errorf("errors %v, want the one of pe-2", errs)
	}
}
