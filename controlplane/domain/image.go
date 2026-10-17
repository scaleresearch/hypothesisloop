package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// RegistryLogin is a login to one registry host, good until its ExpiresAt.
type RegistryLogin = clusterapi.RegistryLogin

// ExperimentImage is the image a platform experiment's jobs run in: Image, repository@digest, of
// one Architecture, recorded at SetAt. A job submitted later runs in it on a node of that
// architecture.
type ExperimentImage struct {
	Image        string    `json:"image" doc:"The image, repository@digest. Every job submitted from now on runs in it."`
	Architecture string    `json:"architecture" doc:"The CPU architecture it runs on, as image manifests name it (amd64, arm64). A job runs only on nodes of it."`
	SetAt        time.Time `json:"set_at"`
}

// RegistryCredential is a login that pushes to and pulls from Repository, where the images of
// one platform experiment go: tag an image Repository:<tag> and push it.
type RegistryCredential struct {
	RegistryLogin
	Repository string `json:"repository" doc:"Where this platform experiment's images are pushed: tag an image <repository>:<tag>, as <repository>:<short commit>-<label>."`
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
