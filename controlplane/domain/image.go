package domain

import (
	"fmt"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

// RegistryLogin is a login to one registry host, good until its ExpiresAt.
type RegistryLogin = clusterapi.RegistryLogin

// ExperimentImage is the image a platform experiment's jobs run in.
type ExperimentImage = platformapi.ExperimentImage

// ResolvedImage is what a registry answers for an image reference: Pinned, the reference by
// digest, and the linux architectures the image runs on.
type ResolvedImage struct {
	Pinned        string
	Architectures []string
}

// ReasonNoExperimentImage refuses a job of a platform experiment that recorded no image.
const ReasonNoExperimentImage = "no_experiment_image"

// ReasonImageUnresolved refuses an image whose registry did not say what it holds.
const ReasonImageUnresolved = "image_unresolved"

// ImageArchitecture is the architecture an image is recorded for: asked, one of those the image
// runs on, or else the one it runs on when that is one.
func ImageArchitecture(ref string, runsOn []string, asked string) (string, error) {
	switch {
	case asked != "" && !slices.Contains(runsOn, asked):
		return "", fmt.Errorf("image %s runs on %s, not on %s", ref, strings.Join(runsOn, ", "), asked)
	case asked != "":
		return asked, nil
	case len(runsOn) == 1:
		return runsOn[0], nil
	case len(runsOn) == 0:
		return "", fmt.Errorf("image %s names no linux architecture it runs on", ref)
	}
	return "", fmt.Errorf("image %s runs on %s: state the architecture its jobs run on (hl image set REF --architecture A)", ref, strings.Join(runsOn, ", "))
}

// NodeArchitectureLabel is on every node a runtime reports, naming its architecture.
const NodeArchitectureLabel = clusterapi.NodeArchitectureLabel
