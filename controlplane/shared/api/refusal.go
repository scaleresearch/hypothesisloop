package api

// Refusal is a refusal a client acts on by its reason, in the {"reason","message"} envelope.
type Refusal struct {
	status  int
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *Refusal) Error() string  { return e.Message }
func (e *Refusal) GetStatus() int { return e.status }

func Refuse(status int, reason, message string) *Refusal {
	return &Refusal{status: status, Reason: reason, Message: message}
}
