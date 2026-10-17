package controller

import (
	"context"
	"time"
)

// recordRunHealth writes where each agent of every open or running platform experiment stands to
// the metrics store, each tick: time to its first job, its jobs in flight, its restarts and the
// coordinator's messages to it. One read and one write, whatever the number of runs.
func (c *Controller) recordRunHealth(ctx context.Context) error {
	agents, err := c.stagesStore.RunHealth(ctx)
	if err != nil {
		return err
	}
	return c.observed.RecordRunHealth(ctx, time.Now().UTC(), agents)
}
