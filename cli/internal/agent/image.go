package agent

import (
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

func ImageShow(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("image show", `usage: hl image show [--platform-experiment PE]

The image the platform experiment's jobs run in, by digest, and the architecture they run on.
Every job submitted runs in the image recorded when it was submitted; hl job status shows a
job's own.`, stderr)
	pe := command.ExperimentFlag(fs)
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if *pe = c.Experiment(*pe); *pe == "" || fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "--platform-experiment is required, and nothing else"))
	}
	var out struct {
		Image *platformapi.ExperimentImage `json:"image"`
	}
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(*pe), nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if out.Image == nil {
		fmt.Fprintln(stdout, "no image recorded: record one with hl image set REF")
		return 0
	}
	fmt.Fprintf(stdout, "%s %s set %s\n", out.Image.Image, out.Image.Architecture, out.Image.SetAt.UTC().Format(time.RFC3339))
	return 0
}

func ImageSet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("image set", `usage: hl image set REF [--architecture A] [--platform-experiment PE]

Records REF as the image every job submitted from now on runs in: a public image, or one pushed
to the repository hl image repository names. The platform reads it from its registry and records
it by digest, with the architecture its jobs run on; --architecture chooses one of an image that
runs on several. The image needs sh and git: each job's commit is fetched into /workspace/code at
start and its command runs there. Prints what was recorded.`, stderr)
	pe := command.ExperimentFlag(fs)
	architecture := fs.String("architecture", "", "the architecture its jobs run on, for an image that runs on several (amd64, arm64)")
	ref, err := command.ParseOne(fs, args, "REF", "")
	if err != nil {
		return command.ParseExit(err)
	}
	if *pe = c.Experiment(*pe); *pe == "" || ref == "" {
		return command.ParseExit(command.Misuse(fs, "REF and --platform-experiment are required"))
	}
	var out platformapi.ExperimentImage
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(*pe)+"/image", map[string]string{"image": ref, "architecture": *architecture}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s %s\n", out.Image, out.Architecture)
	return 0
}
