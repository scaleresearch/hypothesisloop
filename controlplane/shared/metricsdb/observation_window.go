package metricsdb

import "time"

// MaxObservationClockSkew is the deployment's assumed bound on the difference between the
// Postgres clock that stamps an experiment's created_at and the cluster clocks that stamp its
// observations. It is an operational invariant, not a cushion: a cluster whose clock runs
// further behind than this loses its earliest observations from every measurement, silently
// underbilling, and no query can detect samples excluded before the window started.
const MaxObservationClockSkew = 10 * time.Minute

// ObservationWindowStart is the single lookback rule for every job-scoped observation query:
// scan from the moment the experiment's row came into existence.
//
// A job cannot be observed before it exists, so this is simultaneously the tightest bound and
// the only one that never truncates a real observation — a fixed horizon does, and it does so
// silently, giving one subsystem a smaller answer than another about the same job. Deriving the
// window from a column every caller already holds is what makes settlement, live running-cost,
// the controller and the scheduler incapable of disagreeing.
func ObservationWindowStart(createdAt time.Time) time.Time {
	return createdAt.Add(-MaxObservationClockSkew).UTC()
}
