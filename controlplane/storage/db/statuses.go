package db

import "github.com/scaleresearch/hypothesisloop/controlplane/domain"

// unfinishedSQL and endedSQL are, as SQL lists, the stored statuses of a job that has not ended
// and of one that has. No row holds domain.StatusCancelled.
const (
	unfinishedSQL = `('` + string(domain.StatusQueued) + `', '` + string(domain.StatusSubmitted) + `', '` + string(domain.StatusRunning) + `')`
	endedSQL      = `('` + string(domain.StatusCompleted) + `', '` + string(domain.StatusFailed) + `', '` + string(domain.StatusEvicted) + `', '` + string(domain.StatusRejected) + `')`
)
