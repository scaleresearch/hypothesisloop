package platformapi

import "time"

// Change is one announcement of a write: what kind of thing changed, which thing, its new value,
// one qualifying detail and the two ids a subscription is scoped by.
type Change struct {
	Kind                 string `json:"kind"`
	Subject              string `json:"subject"`
	Value                string `json:"value"`
	Detail               string `json:"detail"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id"`
}

// Event is one change as GET /watch delivers it, described as small as it can be described. Never
// a copy of the row: a client that wants detail follows with a normal GET.
type Event struct {
	// ID is the change's place in the stream: every change with a smaller id committed before it.
	// A client reads on from the id of the last change it read.
	ID                   int64  `json:"id"`
	Kind                 string `json:"kind"`
	Subject              string `json:"subject"`
	Value                string `json:"value"`
	Detail               string `json:"detail"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id"`
	// OccurredAt is when the write that made the change began.
	OccurredAt time.Time `json:"occurred_at"`
}

// Subscription is what one stream is subscribed to, derived from the filter the server matches
// against, never from what the client asked for.
type Subscription struct {
	ProjectID            string   `json:"project_id,omitempty"`
	PlatformExperimentID string   `json:"platform_experiment_id"`
	ExperimentID         string   `json:"experiment_id"`
	AgentID              string   `json:"agent_id"`
	Kinds                []string `json:"kinds"`
	// Decisions is set on a stream narrowed to what may need a decision.
	Decisions bool `json:"decisions,omitempty"`
	// After is the change id the stream reads on from.
	After int64 `json:"after"`
}

// Progress is the frame that ends every read of a stream: a client that saved Next reads on with
// after=Next and misses nothing and repeats nothing.
type Progress struct {
	Next     int64 `json:"next"`
	CaughtUp bool  `json:"caught_up"`
}

// Frame is any frame of GET /watch: the first is the subscription, then each change, each read
// ending with its progress. Exactly one of them is set.
type Frame struct {
	Subscription *Subscription `json:"subscription,omitempty"`
	*Event
	*Progress
}

// ResetKind is the kind of the line hl watch prints in place of the changes a saved place no
// longer holds: whoever reads it re-reads state.
const ResetKind = "watch.reset"
