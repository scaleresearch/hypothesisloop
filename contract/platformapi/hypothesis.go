package platformapi

// LinkType is how one hypothesis relates to another. builds_on (refines, extends or combines;
// acyclic, and every hypothesis has at least one) may also point at the objective root, named by
// the platform experiment id. contradicts and related only connect hypotheses.
type LinkType string

const (
	LinkBuildsOn    LinkType = "builds_on"
	LinkContradicts LinkType = "contradicts"
	LinkRelated     LinkType = "related"
)

// LinkTypes is every link type.
var LinkTypes = []LinkType{LinkBuildsOn, LinkContradicts, LinkRelated}

// LinkSpec is a link a caller asks for: from the hypothesis being written to To, a hypothesis id
// or the platform experiment id meaning the objective root.
type LinkSpec struct {
	Type LinkType `json:"type"`
	To   string   `json:"to"`
}

// RegisterHypothesisRequest is the body of POST /hypotheses.
type RegisterHypothesisRequest struct {
	AgentID              string     `json:"agent_id,omitempty" required:"false" doc:"The agent whose hypothesis this is. Absent, it is the caller's own: an agent token's agent, and for any other caller no agent."`
	PlatformExperimentID string     `json:"platform_experiment_id"`
	Text                 string     `json:"text"`
	Links                []LinkSpec `json:"links,omitempty" required:"false" doc:"Where the hypothesis sits: at least one {type: builds_on, to: <hypothesis id or the platform_experiment_id for the objective>}; contradicts and related links to hypotheses are optional."`
	DistinctFrom         []string   `json:"distinct_from,omitempty" required:"false" doc:"Ids of hypotheses a 409 listed that you read and judged a different claim from yours."`
}

// HypothesisStatus is the owning agent's own verdict on its claim. Only the agent that owns the
// hypothesis may change it. A hypothesis a person submitted names no owner, so any agent that
// tested it may settle it, and the findings underneath are the record of who earned the verdict.
type HypothesisStatus string

const (
	// HypothesisOpen is the default: still being tested, no verdict yet.
	HypothesisOpen HypothesisStatus = "open"
	// HypothesisConfirmed means the owning agent has validated the claim as a real improvement —
	// worth another agent building on it.
	HypothesisConfirmed HypothesisStatus = "confirmed"
	// HypothesisRefuted means the owning agent has confidently established the claim does NOT
	// hold — a real negative result, not noise. Worth another agent reading so it isn't retested,
	// but not something to build on.
	HypothesisRefuted HypothesisStatus = "refuted"
	// HypothesisInconclusive means noisy, ambiguous, or not worth another agent's time drilling
	// into further.
	HypothesisInconclusive HypothesisStatus = "inconclusive"
)

// HypothesisStatuses is every status of a hypothesis, the open one first.
var HypothesisStatuses = []HypothesisStatus{HypothesisOpen, HypothesisConfirmed, HypothesisRefuted, HypothesisInconclusive}
