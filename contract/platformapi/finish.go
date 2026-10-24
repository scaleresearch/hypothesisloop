package platformapi

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

// Label is how because heads the reason the platform records.
func (b FinishBecause) Label() string {
	switch b {
	case FinishBlocked:
		return "Blocked"
	case FinishNothingLeft:
		return "Nothing left worth a job"
	}
	return string(b)
}
