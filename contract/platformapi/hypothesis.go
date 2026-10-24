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

// HypothesisStatus is the owning agent's own verdict on its claim, or a researcher's curation of
// an open one (merged, retired). Only the agent that owns the hypothesis may give the verdict. A
// hypothesis a person or a researcher submitted names no owner, so any agent that tested it may
// settle it, and the findings underneath are the record of who earned the verdict.
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
	// HypothesisMerged means a researcher found it states the claim of another hypothesis,
	// merged_into, which now holds the links that pointed at it. No verdict: it is tested under
	// merged_into.
	HypothesisMerged HypothesisStatus = "merged"
	// HypothesisRetired means a researcher took an open idea off the frontier, saying why in
	// status_reason. No verdict: nothing showed it right or wrong, and it takes no job.
	HypothesisRetired HypothesisStatus = "retired"
)

// HypothesisStatuses is every status of a hypothesis, the open one first.
var HypothesisStatuses = []HypothesisStatus{HypothesisOpen, HypothesisConfirmed, HypothesisRefuted, HypothesisInconclusive, HypothesisMerged, HypothesisRetired}

// VerdictStatuses are the statuses a verdict sets: every one but those a researcher's curation
// gives.
var VerdictStatuses = []HypothesisStatus{HypothesisOpen, HypothesisConfirmed, HypothesisRefuted, HypothesisInconclusive}

// MergeHypothesisRequest is the body of POST /hypotheses/{id}/merge.
type MergeHypothesisRequest struct {
	AgentID string `json:"agent_id,omitempty" required:"false" doc:"The researcher that merges. Absent, the agent token's own."`
	Into    string `json:"into" doc:"The hypothesis that states the same claim and keeps it."`
	Reason  string `json:"reason,omitempty" required:"false" doc:"Why the two are one claim."`
}

// RetireHypothesisRequest is the body of POST /hypotheses/{id}/retire.
type RetireHypothesisRequest struct {
	AgentID string `json:"agent_id,omitempty" required:"false" doc:"The researcher that retires it. Absent, the agent token's own."`
	Reason  string `json:"reason" doc:"Why the idea is no longer worth a job."`
}

// The reasons a refusal of curation, or of work on a curated hypothesis, gives.
const (
	// ReasonResearcherOnly refuses a merge, retirement or unlink by anyone but a researcher of
	// the hypothesis's platform experiment.
	ReasonResearcherOnly = "researcher_only"
	// ReasonHypothesisInFlight refuses a merge or retirement of a hypothesis a job is queued or
	// running on.
	ReasonHypothesisInFlight = "hypothesis_in_flight"
	// ReasonHypothesisNotOpen refuses a merge or retirement of a hypothesis that is settled,
	// merged or retired.
	ReasonHypothesisNotOpen = "hypothesis_not_open"
	// ReasonHypothesisMerged and ReasonHypothesisRetired refuse a job, a checkout, a link from or
	// a verdict on a merged or retired hypothesis.
	ReasonHypothesisMerged  = "hypothesis_merged"
	ReasonHypothesisRetired = "hypothesis_retired"
)

// The reasons a refusal of a verdict for whose it is gives.
const (
	// ReasonResearcherGivesNoVerdict refuses a verdict a researcher gives: the agent that
	// tested a hypothesis settles it.
	ReasonResearcherGivesNoVerdict = "researcher_gives_no_verdict"
	// ReasonVerdictNeedsOwnJob refuses a verdict on a hypothesis no agent owns from an agent with
	// no job of its own on it that completed or failed, or one other than open from no agent.
	ReasonVerdictNeedsOwnJob = "verdict_needs_own_job"
)
