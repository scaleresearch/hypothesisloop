package platformapi

import "fmt"

// FinishBecause is why an agent that finishes stops while it still has time and quota: an
// executor with more than a tenth of both left says one, and it heads the reason the platform
// records.
type FinishBecause string

const (
	// FinishBlocked is something the time and quota left cannot fix, which the reason names.
	FinishBlocked FinishBecause = "blocked"
	// FinishNothingLeft is no idea left worth a job, the reason saying what was tried.
	FinishNothingLeft FinishBecause = "nothing_left"
)

// Label is how b heads the reason the platform records, and an error for no FinishBecause.
func (b FinishBecause) Label() (string, error) {
	switch b {
	case FinishBlocked:
		return "Blocked", nil
	case FinishNothingLeft:
		return "Nothing left worth a job", nil
	}
	return "", fmt.Errorf("because %q is none of blocked, nothing_left", string(b))
}
