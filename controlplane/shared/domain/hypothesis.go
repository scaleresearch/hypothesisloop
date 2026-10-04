package domain

import (
	"strings"
	"time"
)

// Hypothesis is a registered research claim an agent intends to test, scoped to a single
// platform experiment. Hypotheses live in the graph store (shared/graphdb): each one builds on the
// experiment's objective or on earlier hypotheses, so the pool is a graph rooted at the objective.
// ExperimentMeta.HypothesisID is required and must reference a hypothesis whose
// PlatformExperimentID matches the job's own. A uniqueness constraint on
// (platform_experiment_id, normalized text) makes registering the same claim twice within one
// platform experiment return the existing hypothesis (see NormalizeHypothesisText); the same claim
// in a different platform experiment is a distinct hypothesis.
// The pool is shared with humans: AgentID is the agent whose hypothesis it is, empty on one no
// agent owns: such a row holds no quota and appears in no standings, any agent may test it and
// anyone may settle it. Actor is whose credential registered it, which is another fact: a person
// or an API key may register an agent's hypothesis for it.
type Hypothesis struct {
	ID                   string           `json:"id"`
	AgentID              string           `json:"agent_id"`
	Actor                Actor            `json:"actor"`
	PlatformExperimentID string           `json:"platform_experiment_id"`
	Text                 string           `json:"text"`
	Status               HypothesisStatus `json:"status"`
	CreatedAt            time.Time        `json:"created_at"`
	// StatusChangedAt is when Status last moved; nil while it still holds its initial value.
	StatusChangedAt *time.Time `json:"status_changed_at"`
	// StatusActor is whose credential last moved Status, nil while it still holds its initial
	// value. The verdict is the owning agent's, or anyone's on a hypothesis no agent owns, and
	// this says who gave it: the agent itself, or the person or API key acting for it.
	StatusActor *Actor `json:"status_actor"`
}

// BranchPrefix is what the name of every git branch of a platform experiment starts with. An
// agent's implementation of a hypothesis is one branch under it, and BaselineBranch another.
func BranchPrefix(platformExperimentID string) string { return "hl/" + platformExperimentID + "/" }

// BaselineBranch is the branch the baseline of a platform experiment is measured on.
func BaselineBranch(platformExperimentID string) string {
	return BranchPrefix(platformExperimentID) + "baseline"
}

// IsBaselineAgentBranch is whether a baseline agent's job may run on branch: the baseline branch,
// or one beside it, baseline-<name>, where the agent tries its code as a researcher would. Only a
// job of the baseline branch itself is declared the baseline.
func IsBaselineAgentBranch(platformExperimentID, branch string) bool {
	baseline := BaselineBranch(platformExperimentID)
	if branch == baseline {
		return true
	}
	name, beside := strings.CutPrefix(branch, baseline+"-")
	return beside && name != "" && !strings.ContainsAny(name, "/ \t\n")
}

// Implementation is one agent's code for a hypothesis: the branch its jobs ran on and the commit
// the newest of them ran. A hypothesis has as many as there are agents that ran it, and none is
// stored: they are read off the jobs.
type Implementation struct {
	AgentID string `json:"agent_id"`
	Branch  string `json:"branch"`
	CodeRef string `json:"code_ref" doc:"The commit the newest job on this branch ran, repository_url@sha."`
}

// HypothesisStatus is the owning agent's own verdict on its claim — mirrors what agents already
// wrote informally into finding/comment text (REFUTED/BLOCKED-style labels), made a real
// filterable field instead. Only the agent named in Hypothesis.AgentID may change it (see
// registry.Service.SetHypothesisStatus) — it's "own your claims," not a global judgment call
// another agent or the operator gets to make on someone else's hypothesis. A human-submitted row
// names no owner, so that rule leaves it to whoever tested it: any agent may settle it, and the
// findings underneath are the record of who earned the verdict.
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

// ValidHypothesisStatus reports whether s is one of the four recognized statuses.
func ValidHypothesisStatus(s HypothesisStatus) bool {
	switch s {
	case HypothesisOpen, HypothesisConfirmed, HypothesisRefuted, HypothesisInconclusive:
		return true
	default:
		return false
	}
}

// LinkType is how one hypothesis relates to another. builds_on (refines, extends or combines;
// acyclic, and every hypothesis has at least one) may also point at the objective root, named by
// the platform experiment id. contradicts and related only connect hypotheses.
type LinkType string

const (
	LinkBuildsOn    LinkType = "builds_on"
	LinkContradicts LinkType = "contradicts"
	LinkRelated     LinkType = "related"
)

// ValidLinkType reports whether t is one of the three link types.
func ValidLinkType(t LinkType) bool {
	switch t {
	case LinkBuildsOn, LinkContradicts, LinkRelated:
		return true
	default:
		return false
	}
}

// LinkSpec is a link a caller asks for: from the hypothesis being written to To, a hypothesis id
// or the platform experiment id meaning the objective root.
type LinkSpec struct {
	Type LinkType `json:"type"`
	To   string   `json:"to"`
}

// Link is one edge seen from one of its ends. HypothesisID, Text and Status describe the other end;
// when that end is the objective root, HypothesisID is the platform experiment id and Text and
// Status are empty. AgentID is the agent whose link it is, empty when no agent owns it, Actor
// whose credential made it, and CreatedAt when.
type Link struct {
	Type         LinkType  `json:"type"`
	HypothesisID string    `json:"hypothesis_id"`
	Text         string    `json:"text"`
	Status       string    `json:"status"`
	AgentID      string    `json:"agent_id"`
	Actor        Actor     `json:"actor"`
	CreatedAt    time.Time `json:"created_at"`
}

// HypothesisLinks is every edge touching one hypothesis, split by direction.
type HypothesisLinks struct {
	Out []Link `json:"out"`
	In  []Link `json:"in"`
}

// HypothesisListItem is a hypothesis with its place in the graph and cheap activity counts, so a
// bounded list read tells the reader which rows are worth a full detail fetch.
// BuildsOn lists outgoing builds_on targets (the root appears as the platform experiment id);
// ChildCount counts incoming builds_on edges.
type HypothesisListItem struct {
	*Hypothesis
	BuildsOn     []string `json:"builds_on"`
	ChildCount   int      `json:"child_count"`
	FindingCount int      `json:"finding_count"`
	CommentCount int      `json:"comment_count"`
}

// HypothesisStats counts a set of hypotheses by status. A status with no hypothesis is absent.
type HypothesisStats struct {
	Total    int                      `json:"total"`
	ByStatus map[HypothesisStatus]int `json:"by_status"`
}

// GraphEdge is one edge of a platform experiment's hypothesis graph, with who created it and when.
type GraphEdge struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Type      LinkType  `json:"type"`
	AgentID   string    `json:"agent_id"`
	Actor     Actor     `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// HypothesisFinding is the post-run write-up an agent files after one of its jobs testing
// this hypothesis reaches a terminal state (COMPLETED, FAILED, EVICTED, or REJECTED).
// Attached to the hypothesis (not just the job) so the accumulated evidence for a claim is
// visible in one place to every agent considering testing it again — see
// services/scheduler.WriteExperimentSummary. One finding per job
// (ExperimentID), but a hypothesis accumulates one per job that tested it.
type HypothesisFinding struct {
	ID                   string `json:"id"`
	HypothesisID         string `json:"hypothesis_id"`
	PlatformExperimentID string `json:"platform_experiment_id"`
	ExperimentID         string `json:"experiment_id"`
	AgentID              string `json:"agent_id"`
	// Actor is whose credential filed the finding. AgentID is the job's agent, whose finding it is.
	Actor     Actor     `json:"actor"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

// HypothesisComment is a freeform, job-independent note on a hypothesis — amending, abandoning,
// or revising a claim, or cross-referencing another hypothesis's finding — recorded without
// requiring a terminal job. Contrast with HypothesisFinding, which is the measured write-up
// bound to one finished job: a comment records a thought, a finding records a result. The two
// must not overlap — recording the same conclusion as both re-pollutes the shared idea pool.
// AgentID is the agent whose note it is, empty on one no agent owns. Actor is whose credential
// wrote it, which is another fact: a person or an API key may write an agent's note for it.
type HypothesisComment struct {
	ID                   string    `json:"id"`
	HypothesisID         string    `json:"hypothesis_id"`
	PlatformExperimentID string    `json:"platform_experiment_id"`
	AgentID              string    `json:"agent_id"`
	Actor                Actor     `json:"actor"`
	Text                 string    `json:"text"`
	CreatedAt            time.Time `json:"created_at"`
}

// NormalizeHypothesisText collapses a hypothesis statement to a canonical form (lowercased,
// whitespace-collapsed, trimmed) so that trivially-different phrasings of the same claim
// ("Model X improves accuracy" vs "model x improves accuracy.") collide on the same
// registered hypothesis instead of creating near-duplicate rows.
func NormalizeHypothesisText(text string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	return strings.Join(fields, " ")
}

// SimilarHypothesis is an existing hypothesis a registration has to be told apart from before it
// is created: its text reads close to the new one, or the new one builds on it and it is refuted.
type SimilarHypothesis struct {
	ID       string           `json:"id"`
	AgentID  string           `json:"agent_id,omitempty"`
	Actor    Actor            `json:"actor"`
	Status   HypothesisStatus `json:"status"`
	Text     string           `json:"text"`
	BuildsOn []string         `json:"builds_on"`
	Reason   string           `json:"reason" enum:"close_text,refuted_parent"`
}
