package platformapi

import "time"

// ExperimentImage is the image a platform experiment's jobs run in: Image, repository@digest, of
// one Architecture, recorded at SetAt. A job submitted later runs in it on a node of that
// architecture.
type ExperimentImage struct {
	Image        string    `json:"image" doc:"The image, repository@digest. Every job submitted from now on runs in it."`
	Architecture string    `json:"architecture" doc:"The CPU architecture it runs on, as image manifests name it (amd64, arm64). A job runs only on nodes of it."`
	SetAt        time.Time `json:"set_at"`
}

// RegistryCredential is a login to Registry, good until ExpiresAt, that pushes to and pulls from
// Repository, where the images of one platform experiment go: tag an image Repository:<tag> and
// push it.
type RegistryCredential struct {
	Registry   string    `json:"registry"`
	Username   string    `json:"username"`
	Password   string    `json:"password"`
	ExpiresAt  time.Time `json:"expires_at"`
	Repository string    `json:"repository" doc:"Where this platform experiment's images are pushed: tag an image <repository>:<tag>, as <repository>:<short commit>-<label>."`
}
