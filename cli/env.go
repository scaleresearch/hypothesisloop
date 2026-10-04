package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

var envCommands = []subcommand{
	{"build", "start building HEAD's .hypothesisloop/Dockerfile before the job that runs in it", envBuild, asAgent},
}

func envBuild(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("env build", `usage: hl env build [FILE] [--image IMAGE] [--hypothesis ID] [--platform-experiment PE]

Starts building the environment HEAD's commit declares in .hypothesisloop/Dockerfile, on the
image its job will name, so that the job finds it built, or building, when it is submitted: a
job of the same commit naming the same image runs in this one build. Commit the file first; the
commit is pushed as hl job submit pushes it. Run it as soon as the file is committed, then write
the rest of the job while it builds. Asking again is safe and builds again a file that failed.

FILE is the job spec the job will be submitted with: its job.image, and metadata.hypothesis_id
for any agent but the baseline. --image and --hypothesis set them without one. Prints the
environment: its key, state (building, ready, failed) and a message saying where the build is,
or the end of its output once the file failed.

Refusals: no_environment_file (the commit has no .hypothesisloop/Dockerfile);
invalid_environment_file; no_environment_builder; unknown_job_image; code_ref_not_in_repository.`, stderr)
	image := fs.String("image", "", "the image the job will name, job.image of FILE by default")
	hypothesis := fs.String("hypothesis", "", "the hypothesis the job will test, metadata.hypothesis_id of FILE by default")
	pe := experimentFlag(fs)
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) > 1 {
		return parseExit(misuse(fs, "want at most one FILE, got %d", len(operands)))
	}
	metadata := map[string]any{}
	if len(operands) == 1 {
		body, err := readYAMLBody(operands[0])
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
		if m, ok := body["metadata"].(map[string]any); ok {
			metadata = m
		}
		if job, ok := body["job"].(map[string]any); ok && *image == "" {
			*image, _ = job["image"].(string)
		}
	}
	if *image == "" {
		return parseExit(misuse(fs, "no image: name the job spec FILE or --image IMAGE, the image the job will run on"))
	}
	if *hypothesis != "" {
		metadata["hypothesis_id"] = *hypothesis
	}
	if v, _ := metadata["platform_experiment_id"].(string); *pe == "" && v != "" {
		*pe = v
	}
	metadata["platform_experiment_id"] = experimentOf(c, *pe)
	if err := fillCodeRef(metadata, ""); err != nil {
		return fail(stderr, fmt.Errorf("env build: %w", err))
	}
	request := map[string]any{"platform_experiment_id": metadata["platform_experiment_id"], "code_ref": metadata["code_ref"], "image": *image}
	var out struct {
		Key     string `json:"key"`
		State   string `json:"state"`
		Base    string `json:"base"`
		Image   string `json:"image,omitempty"`
		Fault   string `json:"fault,omitempty"`
		Message string `json:"message"`
	}
	if err := c.Do("POST", "/environments", request, &out); err != nil {
		return fail(stderr, err)
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	if out.State == "failed" {
		return 1
	}
	return 0
}
