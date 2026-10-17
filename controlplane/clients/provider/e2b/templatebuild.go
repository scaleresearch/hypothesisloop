package e2b

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/imagedigest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// images reads images from their registries (imagedigest.Resolver).
type images interface {
	Resolve(ctx context.Context, ref string, login *domain.RegistryLogin) (imagedigest.Image, error)
	Config(ctx context.Context, ref, architecture string, login *domain.RegistryLogin) (ocispec.ImageConfig, error)
}

// errBuilding is a Create that found the template of its image and size not built yet. The
// build runs at the vendor, and a later Create starts the sandbox once it is ready.
var errBuilding = errors.New("its template is being built")

// vendorTemplate is a template as the vendor lists it.
type vendorTemplate struct {
	TemplateID  string   `json:"templateID"`
	BuildID     string   `json:"buildID"`
	BuildStatus string   `json:"buildStatus"`
	Aliases     []string `json:"aliases"`
}

// readyTemplate is the name of the built template a sandbox of spec boots from. A template that
// does not exist is built: the build is started and the Create fails with errBuilding, as it does
// while the build runs, so a pass is never held for a build and a second Create of the same image
// and size, in this process or another, finds the build the first one started.
func (p *Provider) readyTemplate(ctx context.Context, spec provider.Spec) (string, error) {
	pinned, digest, err := p.pin(ctx, spec.Image)
	if err != nil {
		return "", fmt.Errorf("e2b: reading the digest of image %q: %w", spec.Image, err)
	}
	asked := sizeOf(spec)
	name := templateName(digest, asked)
	var templates []vendorTemplate
	if _, err := p.call(ctx, http.MethodGet, "/templates", nil, &templates); err != nil {
		return "", err
	}
	for _, t := range templates {
		if !slices.Contains(t.Aliases, name) {
			continue
		}
		switch t.BuildStatus {
		case "ready":
			return name, nil
		case "error":
			return "", fmt.Errorf("e2b: the build of template %s (%s) of image %s failed: %s; it is built again once the template is deleted",
				name, t.TemplateID, pinned, p.buildFailure(ctx, t))
		}
		return "", fmt.Errorf("e2b: template %s of image %s with %d cores and %d MiB of memory is %s: %w", name, pinned, asked.CPUs, asked.MemoryMB, t.BuildStatus, errBuilding)
	}
	if err := p.build(ctx, pinned, name, asked); err != nil {
		return "", fmt.Errorf("e2b: building template %s of image %s: %w", name, pinned, err)
	}
	return "", fmt.Errorf("e2b: template %s of image %s with %d cores and %d MiB of memory was missing and its build has started: %w", name, pinned, asked.CPUs, asked.MemoryMB, errBuilding)
}

// pin is image by digest, and the digest. An image named by a tag is pinned to the digest its
// registry answers for the tag now, so a tag pushed again gets a template of its own.
func (p *Provider) pin(ctx context.Context, image string) (pinned, digest string, err error) {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", "", err
	}
	if digested, ok := named.(reference.Digested); ok {
		return image, digested.Digest().String(), nil
	}
	resolved, err := p.images.Resolve(ctx, image, nil)
	if err != nil {
		return "", "", err
	}
	named, err = reference.ParseNormalizedNamed(resolved.Pinned)
	if err != nil {
		return "", "", err
	}
	digested, ok := named.(reference.Digested)
	if !ok {
		return "", "", fmt.Errorf("the registry pinned it as %q, which names no digest", resolved.Pinned)
	}
	return resolved.Pinned, digested.Digest().String(), nil
}

// build starts the build of template name from pinned at size s. E2B takes the image's files and
// drops its config, so the image's ENV, WORKDIR and USER are read from the registry and added as
// build steps, which needs base64 in the image. ENTRYPOINT and CMD are not needed: the adapter
// starts the command it is given.
func (p *Provider) build(ctx context.Context, pinned, name string, s size) error {
	config, err := p.images.Config(ctx, pinned, architecture, nil)
	if err != nil {
		return err
	}
	steps, err := buildSteps(pinned, config)
	if err != nil {
		return err
	}
	var created struct {
		TemplateID string `json:"templateID"`
		BuildID    string `json:"buildID"`
	}
	if _, err := p.call(ctx, http.MethodPost, "/v3/templates", map[string]any{"name": name, "cpuCount": s.CPUs, "memoryMB": s.MemoryMB}, &created); err != nil {
		return err
	}
	if created.TemplateID == "" || created.BuildID == "" {
		return fmt.Errorf("creating the template answered without a template or a build id")
	}
	start := map[string]any{"fromImage": pinned, "steps": steps}
	if _, err := p.call(ctx, http.MethodPost, "/v2"+buildPath(created.TemplateID, created.BuildID), start, nil); err != nil {
		return err
	}
	p.log("e2b: building template %s (%s) of %s with %d cores and %d MiB of memory", name, created.TemplateID, pinned, s.CPUs, s.MemoryMB)
	return nil
}

func buildPath(templateID, buildID string) string {
	return "/templates/" + url.PathEscape(templateID) + "/builds/" + url.PathEscape(buildID)
}

// buildFailure is why the vendor says the template's build failed.
func (p *Provider) buildFailure(ctx context.Context, t vendorTemplate) string {
	var status struct {
		Reason struct {
			Message string `json:"message"`
		} `json:"reason"`
	}
	if _, err := p.call(ctx, http.MethodGet, buildPath(t.TemplateID, t.BuildID)+"/status?limit=1", nil, &status); err != nil {
		return fmt.Sprintf("the reason could not be read: %v", err)
	}
	if status.Reason.Message == "" {
		return "no reason given"
	}
	return status.Reason.Message
}

type buildStep struct {
	Type string   `json:"type"`
	Args []string `json:"args"`
}

var variableName = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*\z`)

// buildSteps are the build steps that give a template what the image's config said. A template
// keeps an ENV step for the build only, so the image's environment goes into a file the adapter's
// supervisor reads before it sets the command's own (supervise.go). Every value is in single
// quotes, inside which a shell reads each byte as itself. USER comes last: the file and the
// working directory are made by root.
func buildSteps(image string, config ocispec.ImageConfig) ([]buildStep, error) {
	var file strings.Builder
	for _, entry := range config.Env {
		name, value, _ := strings.Cut(entry, "=")
		// A name is written into the file unquoted, and one that is not a variable name would be
		// read by the shell of every sandbox as a command.
		if !variableName.MatchString(name) {
			return nil, fmt.Errorf("%s sets %q in its ENV, which is not an environment variable name", image, name)
		}
		fmt.Fprintf(&file, "export %s='%s'\n", name, strings.ReplaceAll(value, "'", `'\''`))
	}
	if file.Len() == 0 {
		file.WriteString("\n")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(file.String()))
	built := []buildStep{{"RUN", []string{"echo " + encoded + " | base64 -d > " + imageEnvPath + " && chmod 644 " + imageEnvPath, "root"}}}
	if config.WorkingDir != "" {
		built = append(built, buildStep{"WORKDIR", []string{config.WorkingDir}})
	}
	if config.User != "" {
		built = append(built, buildStep{"USER", []string{config.User}})
	}
	return built, nil
}
