package domain

import (
	"fmt"
	"time"
)

// ChangesRetention is how long a change is held for GET /watch. A watcher further behind re-reads
// state and watches from the head.
const ChangesRetention = 7 * 24 * time.Hour

// Change is one announcement written in the transaction of the write it announces, and delivered
// as an Event with the id Postgres gives it.
type Change struct {
	Kind                 string
	Subject              string
	Value                string
	Detail               string
	PlatformExperimentID string
	AgentID              string
}

// Validate refuses a change of no kind EventKinds has, or of no platform experiment.
func (c Change) Validate() error {
	if c.PlatformExperimentID == "" {
		return fmt.Errorf("change %s of %s names no platform experiment", c.Kind, c.Subject)
	}
	for _, k := range EventKinds {
		if k.Kind == c.Kind {
			return nil
		}
	}
	return fmt.Errorf("change of %s: %q is no event kind", c.Subject, c.Kind)
}

// IdleNew and IdleContinues are the values of agent.idle and capacity.idle: the first notice of a
// stretch, and each after it.
const (
	IdleNew       = "new"
	IdleContinues = "continues"
)

// IdleValue is the value of the notice of an idle stretch told repeat times before.
func IdleValue(repeat int) string {
	if repeat == 0 {
		return IdleNew
	}
	return IdleContinues
}
