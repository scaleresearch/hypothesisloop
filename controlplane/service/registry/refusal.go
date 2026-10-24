package registry

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

const similarDetail = "Nothing was registered: your hypothesis reads close to the ones in `problems`, each named by its id in field " +
	"(reason close_text), or builds on a refuted one (refuted_parent). Read each. If one states the same claim as " +
	"yours, register nothing and test it under its id; only its owner sets its status. A narrower, broader or " +
	"otherwise different claim is yours to register: resubmit naming each one, as a contradicts link when yours " +
	"cannot hold together with it, else in distinct_from (hl: --distinct-from ID). A refuted_parent stays your " +
	"parent: keep the builds_on link and name it in distinct_from when your claim changes what refuted it; do not " +
	"move your hypothesis onto the objective to get past this."

const closedDetail = "the platform experiment is closed, and its hypothesis graph takes no more hypotheses, links, verdicts or comments from anyone"

// WriteRefusal maps the errors a hypothesis write can return to the status an agent can act on,
// keeping the message.
func (s *Service) WriteRefusal(err error) error {
	var similar *graphdb.SimilarError
	if errors.As(err, &similar) {
		return similarHypotheses(similar.Similar)
	}
	var unsupported *UnsupportedVerdictError
	if errors.As(err, &unsupported) {
		return refusal.Conflict("verdict_unsupported", unsupported.Detail)
	}
	var inRecord *InRecordError
	if errors.As(err, &inRecord) {
		return refusal.Refuse(http.StatusConflict, "hypothesis_in_record", inRecord.Error())
	}
	var curated *graphdb.CuratedError
	if errors.As(err, &curated) {
		return refusal.Refuse(http.StatusConflict, CuratedReason(curated.Hypothesis), curated.Error())
	}
	switch {
	case errors.Is(err, ErrNotResearcher):
		return refusal.Refuse(http.StatusForbidden, platformapi.ReasonResearcherOnly, err.Error())
	case errors.Is(err, ErrResearcherGivesNoVerdict):
		return refusal.Refuse(http.StatusForbidden, platformapi.ReasonResearcherGivesNoVerdict, err.Error())
	case errors.Is(err, ErrVerdictNeedsOwnJob):
		return refusal.Refuse(http.StatusForbidden, platformapi.ReasonVerdictNeedsOwnJob, err.Error())
	case errors.Is(err, ErrChecksOutNothing):
		return refusal.Refuse(http.StatusForbidden, platformapi.ReasonResearcherChecksOutNothing, err.Error())
	case errors.Is(err, ErrHypothesisInFlight):
		return refusal.Refuse(http.StatusConflict, platformapi.ReasonHypothesisInFlight, err.Error())
	case errors.Is(err, graphdb.ErrNotOpen):
		return refusal.Refuse(http.StatusConflict, platformapi.ReasonHypothesisNotOpen, err.Error())
	case errors.Is(err, ErrReasonRequired):
		return refusal.Invalid(refusal.ReasonInvalidRequest, ErrReasonRequired.Error()).On("reason")
	case errors.Is(err, graphdb.ErrDuplicateText):
		return refusal.Refuse(http.StatusConflict, "duplicate_hypothesis", "another hypothesis of this platform experiment already states this text: "+
			"nothing was changed. Test or comment on that one instead")
	case errors.Is(err, graphdb.ErrInvalidLink):
		return refusal.Invalid(refusal.ReasonInvalidRequest, err.Error()).On("links")
	case errors.Is(err, ErrUnknownPlatformExperiment):
		return refusal.Invalid(refusal.ReasonInvalidRequest, err.Error()).On("platform_experiment_id")
	case errors.Is(err, db.ErrUnknownAgent):
		return refusal.Invalid(refusal.ReasonInvalidRequest, err.Error()).On("agent_id")
	case errors.Is(err, ErrBaselineRegistersNoHypothesis):
		return refusal.Refuse(http.StatusForbidden, "baseline_registers_no_hypothesis", err.Error())
	case errors.Is(err, domain.ErrPlatformExperimentClosed):
		return refusal.Refuse(http.StatusConflict, platformapi.ReasonExperimentClosed, closedDetail)
	case errors.Is(err, domain.ErrNotSignedUp):
		return refusal.Refuse(http.StatusForbidden, "not_signed_up", err.Error())
	case errors.Is(err, ErrHypothesisNotOwner):
		return refusal.Refuse(http.StatusForbidden, refusal.ReasonForbidden, err.Error())
	case errors.Is(err, ErrHypothesisNotFound):
		return refusal.Refuse(http.StatusNotFound, refusal.ReasonNotFound, err.Error())
	default:
		return auth.Internal(s.logger, "write hypothesis", err)
	}
}

// LinkRefusal is WriteRefusal for adding one link, whose request names its target in to.
func (s *Service) LinkRefusal(err error) error {
	if errors.Is(err, graphdb.ErrInvalidLink) {
		return refusal.Invalid(refusal.ReasonInvalidRequest, err.Error()).On("to")
	}
	return s.WriteRefusal(err)
}

// MergeRefusal is WriteRefusal for a merge, whose request names the hypothesis it merges into in
// into.
func (s *Service) MergeRefusal(err error) error {
	if errors.Is(err, graphdb.ErrInvalidLink) {
		return refusal.Invalid(refusal.ReasonInvalidRequest, err.Error()).On("into")
	}
	return s.WriteRefusal(err)
}

// CuratedReason is the reason a refusal of work on the merged or retired h gives.
func CuratedReason(h *domain.Hypothesis) string {
	if h.Status == domain.HypothesisMerged {
		return platformapi.ReasonHypothesisMerged
	}
	return platformapi.ReasonHypothesisRetired
}

// similarHypotheses is the 409 a registration gets while it reads close to hypotheses it has not
// been told apart from: each of them a problem, its id the field, close_text or refuted_parent
// the reason.
func similarHypotheses(similar []domain.SimilarHypothesis) error {
	refused := refusal.Conflict(platformapi.ReasonSimilarHypotheses, similarDetail)
	refused.Problems = make([]platformapi.Problem, len(similar))
	for i, h := range similar {
		refused.Problems[i] = platformapi.Problem{Field: h.ID, Reason: h.Reason, Message: fmt.Sprintf("%s, by %s: %s", h.Status, h.Actor.Name, h.Text)}
	}
	return refused
}
