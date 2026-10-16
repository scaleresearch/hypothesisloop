package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/repotemplate"
)

var envCommands = []subcommand{
	{"build", "start building HEAD's .hypothesisloop/Dockerfile before the job that runs in it", envBuild},
	{"status", "say whether HEAD's .hypothesisloop/Dockerfile is building, built or failed, building nothing", envStatus},
	{"stage", "pull the image a job runs in, the baseline's by default, onto this machine for hl env run", envStage},
	{"run", "-- COMMAND: run COMMAND here inside the staged image", envRun},
}

func envBuild(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env build", `usage: hl env build [FILE] [--image IMAGE] [--hypothesis ID] [--platform-experiment PE]

Starts building the environment HEAD's commit declares in .hypothesisloop/Dockerfile, on the
image its job will name, so that the job finds it built, or building, when it is submitted: a
job of the same commit naming the same image runs in this one build. Commit the file first; the
commit is pushed as hl job submit pushes it. Run it as soon as the file is committed, then write
the rest of the job while it builds. Asking again is safe and builds again a file that failed.

FILE is the job spec the job will be submitted with: its job.image, and metadata.hypothesis_id
for any agent but the baseline. It defaults to the repository's .hypothesisloop/job.yaml when
that exists. --image and --hypothesis set them, and override the file's. Prints the
environment: its key, state (building, loaded, ready, failed) and a message saying where the
build is, or the end of its output once the file failed. loaded is built and held on a node of
loaded_cluster while it is pushed: submit its jobs then, and one that does not run there starts
once the push ends. An agent that asks while it builds is told as environment.status on hl
watch once it is loaded, or else once it ends, ready or failed; a failure is told either way.

Refusals: no_environment_file (the commit has no .hypothesisloop/Dockerfile);
invalid_environment_file; no_environment_builder; unknown_job_image; code_ref_not_in_repository.`, stderr)
	image := fs.String("image", "", "the image the job will name, job.image of FILE by default")
	hypothesis := fs.String("hypothesis", "", "the hypothesis the job will test, metadata.hypothesis_id of FILE by default")
	pe := experimentFlag(fs)
	request, code := environmentAsked(c, fs, args, image, hypothesis, pe, "env build", stderr)
	if code >= 0 {
		return code
	}
	var out environmentState
	if err := c.Do("POST", "/environments", request, &out); err != nil {
		return fail(stderr, err)
	}
	return printEnvironment(stdout, out)
}

func envStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env status", `usage: hl env status [FILE] [--image IMAGE] [--hypothesis ID] [--platform-experiment PE]

Prints the environment HEAD's commit declares in .hypothesisloop/Dockerfile, on the image its
job will name, as it is: its key, state (building, loaded, ready, failed) and a message saying
where the build is, or the end of its output once the file failed. It builds nothing: a failed
file stays failed, and a job is never made. FILE, --image and --hypothesis are those of hl env
build. Exits 0 while building, once loaded or ready, 1 once failed.

Refusals: environment_not_requested (nothing asked for its build: hl env build starts it);
no_environment_file; invalid_environment_file.`, stderr)
	image := fs.String("image", "", "the image the job will name, job.image of FILE by default")
	hypothesis := fs.String("hypothesis", "", "the hypothesis the job will test, metadata.hypothesis_id of FILE by default")
	pe := experimentFlag(fs)
	request, code := environmentAsked(c, fs, args, image, hypothesis, pe, "env status", stderr)
	if code >= 0 {
		return code
	}
	var out environmentState
	if err := c.Do("GET", environmentPath(request), nil, &out); err != nil {
		return fail(stderr, err)
	}
	return printEnvironment(stdout, out)
}

// environmentState is an environment as the platform answers it.
type environmentState struct {
	Key           string `json:"key"`
	State         string `json:"state"`
	Base          string `json:"base"`
	Image         string `json:"image,omitempty"`
	LoadedCluster string `json:"loaded_cluster,omitempty"`
	LoadedNode    string `json:"loaded_node,omitempty"`
	Fault         string `json:"fault,omitempty"`
	Message       string `json:"message"`
}

func printEnvironment(stdout io.Writer, out environmentState) int {
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	if out.State == "failed" {
		return 1
	}
	return 0
}

// environmentPath is GET /environments of the environment request names.
func environmentPath(request map[string]any) string {
	query := url.Values{}
	for _, name := range []string{"platform_experiment_id", "code_ref", "image"} {
		if v, _ := request[name].(string); v != "" {
			query.Set(name, v)
		}
	}
	return "/environments?" + query.Encode()
}

// environmentAsked is the request naming the environment HEAD's commit declares for the job of
// FILE, the one operand of args, or of the repository's job spec, with image and hypothesis in
// place of the file's. A code of 0 or more is the exit of a command that cannot ask.
func environmentAsked(c *apiclient.Client, fs *flagSet, args []string, image, hypothesis, pe *string, command string, stderr io.Writer) (map[string]any, int) {
	operands, err := parseOperands(fs, args)
	if err != nil {
		return nil, parseExit(err)
	}
	if len(operands) > 1 {
		return nil, parseExit(misuse(fs, "want at most one FILE, got %d", len(operands)))
	}
	if len(operands) == 0 {
		if root, err := git(".", "rev-parse", "--show-toplevel"); err == nil {
			spec := filepath.Join(root, repotemplate.JobSpec)
			if _, err := os.Stat(spec); err == nil {
				operands = []string{spec}
			}
		}
	}
	metadata := map[string]any{}
	if len(operands) == 1 {
		body, err := readYAMLBody(operands[0])
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return nil, 2
		}
		if m, ok := body["metadata"].(map[string]any); ok {
			metadata = m
		}
		if job, ok := body["job"].(map[string]any); ok && *image == "" {
			*image, _ = job["image"].(string)
		}
	}
	if *image == "" {
		return nil, parseExit(misuse(fs, "no image: name the job spec FILE or --image IMAGE, the image the job will run on"))
	}
	if *hypothesis != "" {
		metadata["hypothesis_id"] = *hypothesis
	}
	if v, _ := metadata["platform_experiment_id"].(string); *pe == "" && v != "" {
		*pe = v
	}
	metadata["platform_experiment_id"] = experimentOf(c, *pe)
	if err := fillCodeRef(metadata, ""); err != nil {
		return nil, fail(stderr, fmt.Errorf("%s: %w", command, err))
	}
	return map[string]any{"platform_experiment_id": metadata["platform_experiment_id"], "code_ref": metadata["code_ref"], "image": *image}, -1
}
