package experimentspec

import "bytes"

// The files every experiment's repository holds, at the paths the platform, hl and the agents'
// briefings name. The experiment template repository holds each of them.
const (
	Readme         = "README.md"
	ExperimentFile = ".hypothesisloop/experiment.yaml"
	JobSpec        = ".hypothesisloop/job.yaml"
	Dockerfile     = ".hypothesisloop/Dockerfile"
	// MetricsHelper posts a job's metric. A repository may leave it out and post the metric
	// itself, which hl repo check says how to do and fails nothing for.
	MetricsHelper = "metrics.py"

	// Placeholder marks what a file of the template leaves to be filled in.
	Placeholder = "REPLACE-WITH"
)

// Unfilled counts the placeholders data still holds: a file made from the template and not yet
// filled in holds at least one.
func Unfilled(data []byte) int { return bytes.Count(data, []byte(Placeholder)) }
