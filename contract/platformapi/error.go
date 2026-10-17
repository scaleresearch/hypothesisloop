package platformapi

import "time"

// ErrorModel is the body of every answer that refuses or fails a request.
type ErrorModel struct {
	Reason   string       `json:"reason,omitempty" doc:"What a client acts on: a code such as not_found, set on every refusal that names one."`
	Message  string       `json:"message" doc:"What was refused, or what failed, in a sentence."`
	Problems []ErrorModel `json:"problems,omitempty" doc:"Each refusal of a check that found several, so that all of them are fixed in one pass."`
	// Summary, OperatorDetail and Since are those of a status reason, on a refusal that answers a
	// condition of the platform rather than of the request.
	Summary        string     `json:"summary,omitempty" doc:"The sentence for a person, on a refusal for a condition of the platform."`
	OperatorDetail string     `json:"operator_detail,omitempty" doc:"What the platform's staff read of that condition."`
	Since          *time.Time `json:"since,omitempty" doc:"Since when that condition holds."`
}
