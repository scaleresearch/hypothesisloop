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
	switch {
	case errors.Is(err, graphdb.ErrDuplicateText):
		return refusal.Refuse(http.StatusConflict, "duplicate_hypothesis", "another hypothesis of this platform experiment already states this text: "+
			"nothing was changed. Test or comment on that one instead")
	case errors.Is(err, graphdb.ErrInvalidLink),
		errors.Is(err, ErrUnknownPlatformExperiment), errors.Is(err, db.ErrUnknownAgent):
		return refusal.Refuse(http.StatusBadRequest, refusal.ReasonInvalidRequest, err.Error())
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
