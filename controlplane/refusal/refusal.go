package refusal

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

// Refusal is a refusal a client acts on by its reason, answered as the API's one error body with
// the status it answers with.
type Refusal struct {
	status int
	platformapi.ErrorModel
}

func (e *Refusal) Error() string  { return e.Message }
func (e *Refusal) GetStatus() int { return e.status }

func Refuse(status int, reason, message string) *Refusal {
	return &Refusal{status: status, ErrorModel: platformapi.ErrorModel{Reason: reason, Message: message}}
}

// Problems collects the refusals of one check, so that it reports every one of them at once.
type Problems []*Refusal

// Add keeps err when it is a refusal, each of its problems when it holds several, and answers
// any other error, which ends the check.
func (p *Problems) Add(err error) error {
	var refused *Refusal
	if err == nil {
		return nil
	}
	if !errors.As(err, &refused) {
		return err
	}
	if len(refused.Problems) > 0 {
		for _, problem := range refused.Problems {
			*p = append(*p, &Refusal{status: refused.status, ErrorModel: problem})
		}
		return nil
	}
	*p = append(*p, refused)
	return nil
}

// Err is nil for no problem, the refusal itself for one, and for several one refusal with the
// first's status and reason, every message numbered, and each in Problems.
func (p Problems) Err() error {
	switch len(p) {
	case 0:
		return nil
	case 1:
		return p[0]
	}
	messages := make([]string, len(p))
	problems := make([]platformapi.ErrorModel, len(p))
	for i, problem := range p {
		messages[i] = fmt.Sprintf("(%d) %s: %s", i+1, problem.Reason, problem.Message)
		problems[i] = problem.ErrorModel
	}
	return &Refusal{status: p[0].status, ErrorModel: platformapi.ErrorModel{Reason: p[0].Reason, Message: fmt.Sprintf("%d problems: %s", len(p), strings.Join(messages, "; ")), Problems: problems}}
}

// NotFound refuses what with 404 not_found.
func NotFound(what string) *Refusal {
	return Refuse(http.StatusNotFound, "not_found", what+" not found")
}

// Invalid refuses with 422 and reason.
func Invalid(reason, message string) *Refusal {
	return Refuse(http.StatusUnprocessableEntity, reason, message)
}

// Conflict refuses with 409 and reason.
func Conflict(reason, message string) *Refusal {
	return Refuse(http.StatusConflict, reason, message)
}
