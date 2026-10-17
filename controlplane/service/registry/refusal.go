package registry

import (
	"errors"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
)

// Rejection is a request the registry refuses with Status, answered in the API's {"error"}
// envelope with the message of Err.
type Rejection struct {
	Status int
	Err    error
}

func (r *Rejection) Error() string  { return r.Err.Error() }
func (r *Rejection) Unwrap() error  { return r.Err }
func (r *Rejection) GetStatus() int { return r.Status }

// similarHypothesesError is the 409 a registration gets while it reads close to hypotheses it has
// not been told apart from: the refusal with the hypotheses themselves beside it.
type similarHypothesesError struct {
	*refusal.Refusal
	Similar []domain.SimilarHypothesis `json:"similar"`
}

const similarDetail = "Nothing was registered: your hypothesis reads close to the ones in `similar` " +
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
		return &similarHypothesesError{Refusal: refusal.Refuse(http.StatusConflict, "similar_hypotheses", similarDetail), Similar: similar.Similar}
	}
	var unsupported *UnsupportedVerdictError
	if errors.As(err, &unsupported) {
		return &Rejection{Status: http.StatusConflict, Err: errors.New(unsupported.Detail)}
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
		return &Rejection{Status: http.StatusBadRequest, Err: err}
	case errors.Is(err, ErrBaselineRegistersNoHypothesis):
		return refusal.Refuse(http.StatusForbidden, "baseline_registers_no_hypothesis", err.Error())
	case errors.Is(err, domain.ErrPlatformExperimentClosed):
		return refusal.Refuse(http.StatusConflict, "experiment_closed", closedDetail)
	case errors.Is(err, domain.ErrNotSignedUp):
		return refusal.Refuse(http.StatusForbidden, "not_signed_up", err.Error())
	case errors.Is(err, ErrHypothesisNotOwner):
		return &Rejection{Status: http.StatusForbidden, Err: err}
	case errors.Is(err, ErrHypothesisNotFound):
		return &Rejection{Status: http.StatusNotFound, Err: err}
	default:
		return auth.Internal(s.logger, "write hypothesis", err)
	}
}
