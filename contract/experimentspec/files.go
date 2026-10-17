package experimentspec

// The files every experiment's repository holds, at the paths the platform, hl and the agents'
// briefings name. The experiment template repository holds each of them.
const (
	Readme         = "README.md"
	ExperimentFile = ".hypothesisloop/experiment.yaml"
	JobSpec        = ".hypothesisloop/job.yaml"
	Environment    = ".hypothesisloop/Dockerfile"
	// MetricsHelper posts a job's metric. A repository may leave it out and post the metric
	// itself, which hl repo check says how to do and fails nothing for.
	MetricsHelper = "metrics.py"

	// Placeholder marks what a file of the template leaves to be filled in.
	Placeholder = "REPLACE-WITH"
)
