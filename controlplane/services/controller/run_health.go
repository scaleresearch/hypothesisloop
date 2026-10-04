package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// recordRunHealth writes where each agent of every open or running platform experiment stands to
// the metrics store, each tick: time to its first job, its jobs in flight, its restarts and the
// coordinator's messages to it. One platform experiment that fails leaves the others written.
func (c *Controller) recordRunHealth(ctx context.Context) error {
	now := time.Now().UTC()
	var errs []error
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := c.stagesStore.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			errs = append(errs, fmt.Errorf("list %s platform experiments: %w", status, err))
			continue
		}
		for _, pe := range pes {
			agents, err := c.stagesStore.RunHealth(ctx, pe.ID)
			if err == nil {
				err = c.observed.RecordRunHealth(ctx, pe.ID, now, agents)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("platform experiment %s: %w", pe.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}
