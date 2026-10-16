package repotemplate

import _ "embed"

// The files every experiment's repository holds, at the paths the platform, hl and the agents'
// briefings name. hl repo check says what makes them ready.
const (
	Readme         = "README.md"
	ExperimentFile = ".hypothesisloop/experiment.yaml"
	JobSpec        = ".hypothesisloop/job.yaml"
	Environment    = ".hypothesisloop/Dockerfile"
	Gitignore      = ".gitignore"
	// MetricsHelper posts a job's metric. A repository may leave it out and post the metric
	// itself, which hl repo check says how to do and fails nothing for.
	MetricsHelper = "metrics.py"

	// Placeholder marks what a file of the template leaves to be filled in.
	Placeholder = "REPLACE-WITH"
)

var (
	//go:embed files/README.md
	readme string
	//go:embed files/.hypothesisloop/experiment.yaml
	experimentFile string
	//go:embed files/.hypothesisloop/job.yaml
	jobSpec string
	//go:embed files/.hypothesisloop/Dockerfile
	environment string
	//go:embed files/.gitignore
	gitignore string
	//go:embed files/metrics.py
	metricsHelper string
)

// Files is the template: what a repository the platform creates starts from and what hl repo
// init adds to one that lacks it, by path.
func Files() map[string]string {
	return map[string]string{Readme: readme, ExperimentFile: experimentFile, JobSpec: jobSpec, Environment: environment, Gitignore: gitignore, MetricsHelper: metricsHelper}
}
