package agent

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

type experimentImage struct {
	Image        string    `json:"image"`
	Architecture string    `json:"architecture"`
	SetAt        time.Time `json:"set_at"`
}

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
		Image *experimentImage `json:"image"`
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
to the repository hl image credentials names. The platform reads it from its registry and records
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
	var out experimentImage
	if err := c.Do("PUT", "/platform-experiments/"+url.PathEscape(*pe)+"/image", map[string]string{"image": ref, "architecture": *architecture}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s %s\n", out.Image, out.Architecture)
	return 0
}

func ImageCredentials(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("image credentials", `usage: hl image credentials [--platform-experiment PE]

A login that pushes the platform experiment's images, as export lines: eval "$(hl image credentials)".
HL_IMAGE_REPOSITORY is where they go; tag an image $HL_IMAGE_REPOSITORY:<short commit>-<label>.
The login lasts about an hour (HL_REGISTRY_EXPIRES_AT); ask again for a new one.
  printf %s "$HL_REGISTRY_PASSWORD" | sudo podman login -u "$HL_REGISTRY_USERNAME" --password-stdin "$HL_REGISTRY"`, stderr)
	pe := command.ExperimentFlag(fs)
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if *pe = c.Experiment(*pe); *pe == "" || fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "--platform-experiment is required, and nothing else"))
	}
	var out struct {
		Registry   string    `json:"registry"`
		Username   string    `json:"username"`
		Password   string    `json:"password"`
		ExpiresAt  time.Time `json:"expires_at"`
		Repository string    `json:"repository"`
	}
	if err := c.Do("POST", "/platform-experiments/"+url.PathEscape(*pe)+"/registry-credentials", nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	for _, v := range [][2]string{
		{"HL_REGISTRY", out.Registry},
		{"HL_REGISTRY_USERNAME", out.Username},
		{"HL_REGISTRY_PASSWORD", out.Password},
		{"HL_REGISTRY_EXPIRES_AT", out.ExpiresAt.UTC().Format(time.RFC3339)},
		{"HL_IMAGE_REPOSITORY", out.Repository},
	} {
		fmt.Fprintf(stdout, "export %s='%s'\n", v[0], strings.ReplaceAll(v[1], "'", `'\''`))
	}
	return 0
}
