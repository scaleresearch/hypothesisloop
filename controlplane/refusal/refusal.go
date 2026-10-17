// Package refusal is the one error the API answers a request with: a status, and the body every
// refusal and failure has, platformapi.ErrorModel.
package refusal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

// Refusal is a request refused or failed, answered with its status and its ErrorModel.
type Refusal struct {
	status int
	platformapi.ErrorModel
}

func (e *Refusal) Error() string  { return e.Message }
func (e *Refusal) GetStatus() int { return e.status }

func Refuse(status int, reason, message string) *Refusal {
	return &Refusal{status: status, ErrorModel: platformapi.ErrorModel{Reason: reason, Message: message}}
}

// On names the field of the request the refusal concerns, as its one problem.
func (e *Refusal) On(field string) *Refusal {
	e.Problems = []platformapi.Problem{{Reason: e.Reason, Message: e.Message, Field: field}}
	return e
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
	if len(refused.Problems) < 2 {
		*p = append(*p, refused)
		return nil
	}
	for _, problem := range refused.Problems {
		*p = append(*p, Refuse(refused.status, problem.Reason, problem.Message).On(problem.Field))
	}
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
	problems := make([]platformapi.Problem, len(p))
	for i, refused := range p {
		messages[i] = fmt.Sprintf("(%d) %s", i+1, refused.Message)
		problems[i] = platformapi.Problem{Reason: refused.Reason, Message: refused.Message}
		if len(refused.Problems) == 1 {
			problems[i].Field = refused.Problems[0].Field
		}
	}
	return &Refusal{status: p[0].status, ErrorModel: platformapi.ErrorModel{Reason: p[0].Reason, Message: fmt.Sprintf("%d problems: %s", len(p), strings.Join(messages, "; ")), Problems: problems}}
}

// NotFound refuses what with 404 not_found.
func NotFound(what string) *Refusal {
	return Refuse(http.StatusNotFound, ReasonNotFound, what+" not found")
}

// Invalid refuses with 422 and reason.
func Invalid(reason, message string) *Refusal {
	return Refuse(http.StatusUnprocessableEntity, reason, message)
}

// Conflict refuses with 409 and reason.
func Conflict(reason, message string) *Refusal {
	return Refuse(http.StatusConflict, reason, message)
}

// ReasonOf is the reason err refuses with, "" for an error that is no refusal.
func ReasonOf(err error) string {
	var refused *Refusal
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// The reasons of the refusals any operation may answer, whatever it does.
const (
	ReasonInvalidRequest   = "invalid_request"
	ReasonUnauthenticated  = "unauthenticated"
	ReasonForbidden        = "forbidden"
	ReasonNotFound         = "not_found"
	ReasonMethodNotAllowed = "method_not_allowed"
	ReasonTooLarge         = "too_large"
	ReasonRateLimited      = "rate_limited"
	ReasonInternal         = "internal"
	ReasonUnavailable      = "unavailable"
)

// InternalMessage is the message of every answer to a failure of the platform's own. What failed
// is logged and never sent: the caller may be anyone.
const InternalMessage = "internal error"

// OfStatus is the refusal of a request with status that names no reason of its own: those huma
// answers itself, and those of a router that serves no such route. A failure of the platform's
// own says nothing of what failed.
func OfStatus(status int, message string) *Refusal {
	switch {
	case status >= 500 && status != http.StatusServiceUnavailable:
		return Refuse(status, ReasonInternal, InternalMessage)
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return Refuse(status, ReasonInvalidRequest, message)
	case status == http.StatusUnauthorized:
		return Refuse(status, ReasonUnauthenticated, message)
	case status == http.StatusTooManyRequests:
		return Refuse(status, ReasonRateLimited, message)
	case status == http.StatusRequestEntityTooLarge:
		return Refuse(status, ReasonTooLarge, message)
	case status == http.StatusServiceUnavailable:
		return Refuse(status, ReasonUnavailable, message)
	}
	return Refuse(status, strings.ReplaceAll(strings.ToLower(http.StatusText(status)), " ", "_"), message)
}

// Write answers e as the API's one error body.
func Write(w http.ResponseWriter, e *Refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(e.ErrorModel)
}

// NoRoute answers a path the router serves nothing at.
func NoRoute(w http.ResponseWriter, r *http.Request) {
	Write(w, Refuse(http.StatusNotFound, ReasonNotFound, "the platform serves nothing at "+r.URL.Path+": a client older or newer than the platform asks for what it does not serve"))
}

// NoMethod answers a method the router does not serve at a path it serves.
func NoMethod(w http.ResponseWriter, r *http.Request) {
	Write(w, Refuse(http.StatusMethodNotAllowed, ReasonMethodNotAllowed, "the platform serves no "+r.Method+" "+r.URL.Path+": a client older or newer than the platform asks for what it does not serve"))
}
