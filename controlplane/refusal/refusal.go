package refusal

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Refusal is a refusal a client acts on by its reason, in the {"reason","message"} envelope.
// Problems holds each refusal of a check that found several, so a client fixes them in one pass.
type Refusal struct {
	status   int
	Reason   string     `json:"reason"`
	Message  string     `json:"message"`
	Problems []*Refusal `json:"problems,omitempty"`
	// Summary, OperatorDetail and Since are those of domain.StatusReason, on a refusal that
	// answers a condition of the platform rather than of the request.
	Summary        string     `json:"summary,omitempty"`
	OperatorDetail string     `json:"operator_detail,omitempty"`
	Since          *time.Time `json:"since,omitempty"`
}

func (e *Refusal) Error() string  { return e.Message }
func (e *Refusal) GetStatus() int { return e.status }

func Refuse(status int, reason, message string) *Refusal {
	return &Refusal{status: status, Reason: reason, Message: message}
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
		*p = append(*p, refused.Problems...)
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
	for i, problem := range p {
		messages[i] = fmt.Sprintf("(%d) %s: %s", i+1, problem.Reason, problem.Message)
	}
	return &Refusal{status: p[0].status, Reason: p[0].Reason, Message: fmt.Sprintf("%d problems: %s", len(p), strings.Join(messages, "; ")), Problems: p}
}
