package repotemplate

import _ "embed"

// The files every experiment's repository holds, at the paths the platform, hl and the agents'
// briefings name. agents/coordinator/experiments/template/experiment-checklist.md says what
// makes them ready.
const (
	Readme         = "README.md"
	ExperimentFile = ".hypothesisloop/experiment.yaml"
	JobSpec        = ".hypothesisloop/job.yaml"
	Gitignore      = ".gitignore"

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
	//go:embed files/.gitignore
	gitignore string
)

// Files is the template: what a repository the platform creates starts from and what hl repo
// init adds to one that lacks it, by path.
func Files() map[string]string {
	return map[string]string{Readme: readme, ExperimentFile: experimentFile, JobSpec: jobSpec, Gitignore: gitignore}
}
