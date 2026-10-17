package refusal

import "errors"

// Rejection is a request refused with Status, answered in the API's {"error"} envelope with the
// message of Err.
type Rejection struct {
	Status int
	Err    error
}

func (r *Rejection) Error() string  { return r.Err.Error() }
func (r *Rejection) Unwrap() error  { return r.Err }
func (r *Rejection) GetStatus() int { return r.Status }

// Reject is the Rejection of message with status.
func Reject(status int, message string) *Rejection {
	return &Rejection{Status: status, Err: errors.New(message)}
}
