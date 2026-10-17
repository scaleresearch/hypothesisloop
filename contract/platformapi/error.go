package platformapi

// ErrorModel is the body of every answer that refuses or fails a request.
type ErrorModel struct {
	Reason   string    `json:"reason" doc:"What a client acts on: a code such as not_found."`
	Message  string    `json:"message" doc:"What was refused, or what failed, in a sentence."`
	Problems []Problem `json:"problems,omitempty" doc:"Each refusal of a check that found several, so that all of them are fixed in one pass."`
}

// Problem is one refusal of a check that found several, naming the field it concerns.
type Problem struct {
	Reason  string `json:"reason" doc:"What a client acts on, as reason of the refusal."`
	Message string `json:"message" doc:"What was refused, in a sentence."`
	Field   string `json:"field,omitempty" doc:"The field of the request, or the thing, the problem concerns."`
}
