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
