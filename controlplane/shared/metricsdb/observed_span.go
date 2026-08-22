package metricsdb

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ObservedSpan is everything the billing and lifecycle paths need to know about when an
// experiment was seen running, derived in one pass over its observations.
type ObservedSpan struct {
	// Observed is false when nothing was recorded for the experiment in the window at all.
	Observed bool
	// First and Last are the earliest and latest real observation.
	First, Last time.Time
	// Total is time the experiment was continuously observed running: the sum of the intervals
	// between consecutive observations, counting an interval only when it is at most gapCap.
	Total time.Duration
	// Stint is the same sum restricted to the run since the last gap longer than gapCap — what a
	// job has accrued since it most recently resumed, which is what a preemption rescale must
	// subtract so an earlier stint is not charged against the estimate twice.
	Stint time.Duration
}

// ObserveSpan measures how long experimentID was observed running in [since, now).
//
// Elapsed is the sum of the intervals between consecutive observations, an interval counting only
// when it is at most gapCap. A longer gap means the job genuinely was not running in between
// (preempted, between pods, node dead) and contributes nothing; the next observation simply starts
// a new stint, so the per-interval cap *is* the stint split and no separate bookkeeping is needed.
//
// Both observation series count and are unioned, not required together: the runtime's liveness
// heartbeat and the job's own reported metrics each independently prove the job was alive at their
// timestamp, so a job between metric reports is not mistaken for stopped and a job that never
// reports a training metric still accrues time from its heartbeat.
//
// This replaces counting points on a PromQL range-query grid. That grid was phased to each
// caller's own window, so the same job measured by settlement, by live running-cost accounting and
// by a stage boundary could differ by a whole step, and re-measuring later could change the
// answer. It also quantised: with a 30s step a job that genuinely ran 25s measured zero and was
// billed nothing. Summing real intervals is exact to the resolution of the observations
// themselves, identical for every caller, and stable however long after the fact it is asked —
// the samples are immutable, so settling promptly and settling an hour later agree.
//
// It measures observed span under the gap rule, not process lifetime: time before the first
// observation, after the last, and across gaps longer than gapCap is deliberately not counted.
// Billing only what was actually observed is the point (important.md #18).
func ObserveSpan(ctx context.Context, dbURL, experimentID string, since, now time.Time, gapCap time.Duration) (ObservedSpan, error) {
	// An inverted window means a corrupt created_at or a caller passing the two the wrong way
	// round. Returning an empty span would bill that as "never observed" — silently, and as a
	// full refund. It is not a measurement this package can make.
	if !now.After(since) {
		return ObservedSpan{}, fmt.Errorf("metricsdb.ObserveSpan: window for %s ends at or before it starts", experimentID)
	}
	if gapCap <= 0 {
		return ObservedSpan{}, fmt.Errorf("metricsdb.ObserveSpan: gap cap must be positive")
	}
	quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	id := quote(experimentID)
	capMillis := gapCap.Milliseconds()
	// DISTINCT before the window function: the two series routinely carry the same instant, and
	// duplicate rows would make multiplicity and tie ordering part of the bill.
	query := fmt.Sprintf(`WITH obs AS (`+
		`SELECT DISTINCT CAST(greptime_timestamp AS BIGINT) AS ts FROM %s WHERE experiment_id = %s AND greptime_timestamp >= %d::TimestampMillisecond AND greptime_timestamp < %d::TimestampMillisecond `+
		`UNION `+
		`SELECT DISTINCT CAST(greptime_timestamp AS BIGINT) AS ts FROM %s WHERE job_id = %s AND greptime_timestamp >= %d::TimestampMillisecond AND greptime_timestamp < %d::TimestampMillisecond`+
		`), d AS (SELECT ts, ts - LAG(ts) OVER (ORDER BY ts) AS gap FROM obs)`+
		`, b AS (SELECT MAX(CASE WHEN gap > %d THEN ts END) AS stint_start FROM d) `+
		`SELECT MIN(d.ts), MAX(d.ts), `+
		`SUM(CASE WHEN d.gap <= %d THEN d.gap ELSE 0 END), `+
		`SUM(CASE WHEN d.gap <= %d AND (b.stint_start IS NULL OR d.ts >= b.stint_start) THEN d.gap ELSE 0 END) `+
		`FROM d CROSS JOIN b`,
		aliveHeartbeatMetric, id, since.UnixMilli(), now.UnixMilli(),
		ExperimentMetricValue, id, since.UnixMilli(), now.UnixMilli(),
		capMillis, capMillis, capMillis,
	)
	row, found, err := querySingleRow(ctx, dbURL, "metricsdb.ObserveSpan", query, 4)
	if err != nil {
		return ObservedSpan{}, err
	}
	// No rows, or a NULL minimum, both mean nothing was observed in the window.
	if !found || row[0] == nil {
		return ObservedSpan{}, nil
	}
	span := ObservedSpan{
		Observed: true,
		First:    time.UnixMilli(int64(*row[0])).UTC(),
		Last:     time.UnixMilli(int64(*row[1])).UTC(),
	}
	if row[2] != nil {
		span.Total = time.Duration(int64(*row[2])) * time.Millisecond
	}
	if row[3] != nil {
		span.Stint = time.Duration(int64(*row[3])) * time.Millisecond
	}
	return span, nil
}
