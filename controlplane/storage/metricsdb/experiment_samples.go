package metricsdb

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExperimentSampleSet is one experiment's recorded metric history: every value sample the job
// reported, and the fraction-complete written alongside each of them. Both halves come back
// together because RecordMetric writes them as a pair at one instant, and only the pair is a
// complete datapoint.
type ExperimentSampleSet struct {
	Values    []VectorSample
	Fractions []VectorSample
}

// ExperimentMetricSamples returns every sample experimentID recorded in [since, until), exactly
// as stored — one entry per POST, in timestamp order.
//
// Read with SQL rather than a PromQL range query on purpose. A range query evaluates the series
// on a fixed grid and reports, at each grid point, the last sample at or before it: samples
// closer together than the step simply never appear, and a single sample is repeated across
// every step that follows it. Every node of a distributed job reports under one label set — a
// rank is not a label — so a three-node job posting one value each within the same second came
// back as a single value repeated for the length of the run. The samples were never lost, only
// resampled away on the way out, which is why the dashboard and the scenarios that read it saw
// one rank where three had reported.
func ExperimentMetricSamples(ctx context.Context, dbURL, experimentID string, since, until time.Time) (ExperimentSampleSet, error) {
	if !until.After(since) {
		return ExperimentSampleSet{}, fmt.Errorf("metricsdb.ExperimentMetricSamples: %s: until does not follow since", experimentID)
	}
	values, err := experimentSamplesFrom(ctx, dbURL, ExperimentMetricValue, experimentID, since, until)
	if err != nil {
		return ExperimentSampleSet{}, err
	}
	fractions, err := experimentSamplesFrom(ctx, dbURL, ExperimentMetricFraction, experimentID, since, until)
	if err != nil {
		return ExperimentSampleSet{}, err
	}
	return ExperimentSampleSet{Values: values, Fractions: fractions}, nil
}

func experimentSamplesFrom(ctx context.Context, dbURL, table, experimentID string, since, until time.Time) ([]VectorSample, error) {
	// Absolute bounds rather than a relative window, for the same reason every other read in
	// this package uses them: the window must be the caller's, not the database's idea of now.
	query := fmt.Sprintf(
		`SELECT * FROM %s WHERE job_id = '%s' `+
			`AND greptime_timestamp >= %d::TimestampMillisecond AND greptime_timestamp < %d::TimestampMillisecond`,
		table, strings.ReplaceAll(experimentID, "'", "''"), since.UnixMilli(), until.UnixMilli())
	samples, err := runClusterSnapshotQuery(ctx, dbURL, query)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.ExperimentMetricSamples: %s: %w", table, err)
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].At.Before(samples[j].At) })
	return samples, nil
}
