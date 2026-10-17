package db

import (
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// The stored statuses of a job as SQL lists: one that has not ended, one that has, one placed on
// a cluster (domain.PlacedStatuses) and one that waits to run (domain.UnstartedStatuses). No row
// holds domain.StatusCancelled. endedSQL and placedSQL are the text of the predicates of the
// partial indexes on experiments in schema.sql, so that a query with them uses those indexes.
var (
	unfinishedSQL = statusList(func(s domain.ExperimentStatus) bool { return !s.Ended() })
	endedSQL      = statusList(func(s domain.ExperimentStatus) bool { return s.Ended() && s != domain.StatusCancelled })
	placedSQL     = statusList(func(s domain.ExperimentStatus) bool { return slices.Contains(domain.PlacedStatuses, s) })
	unstartedSQL  = statusList(func(s domain.ExperimentStatus) bool { return slices.Contains(domain.UnstartedStatuses, s) })
)

// reservedSQL is whether a job row reserves its estimated cost: it has not ended, or it ended and
// its cost is not yet written to the metrics store (quota_settled_at). Admission counts what is
// reserved beside what was observed.
var reservedSQL = `(status IN ` + unfinishedSQL + ` OR (status IN ` + endedSQL + ` AND quota_settled_at IS NULL))`

// statusList is the statuses of domain.ExperimentStatuses that keep holds, in their order, as an
// SQL list.
func statusList(keep func(domain.ExperimentStatus) bool) string {
	var quoted []string
	for _, s := range domain.ExperimentStatuses {
		if keep(s) {
			quoted = append(quoted, "'"+string(s)+"'")
		}
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}
