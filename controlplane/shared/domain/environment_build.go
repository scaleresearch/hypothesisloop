package domain

import "time"

// EnvironmentBuild is a build the control plane assigned to a cluster, as its reconcile answer
// lists it under builds. The cluster builds Dockerfile with the build argument BASE set to Base,
// pushes the image by digest with no tag to Repository, and reports the outcome under Key and
// Attempt. A build that leaves the list is no longer the cluster's to run.
type EnvironmentBuild struct {
	Key        string `json:"key"`
	Attempt    int    `json:"attempt"`
	Base       string `json:"base"`
	Dockerfile string `json:"dockerfile"`
	Repository string `json:"repository"`
}

// RegistryLogin is a login to the registry host Registry, good until ExpiresAt. It is handed out
// to push one build (POST /internal/clusters/{name}/builds/{key}/credential) and, as registry_pull
// of the reconcile answer, to pull the environment images of the jobs the answer lists. It is kept
// in memory and never written to disk beyond the pull secret of one job.
type RegistryLogin struct {
	Registry  string    `json:"registry"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expires_at"`
}

// EnvironmentBuildCredentialRequest asks for the push login of the build of a key the cluster
// was assigned at Attempt.
type EnvironmentBuildCredentialRequest struct {
	Attempt int `json:"attempt"`
}

// EnvironmentBuildOutcomeKind is how a build ended, as the cluster that ran it reports it.
type EnvironmentBuildOutcomeKind string

const (
	// EnvironmentBuildPushed is a build whose image was pushed, by Digest.
	EnvironmentBuildPushed EnvironmentBuildOutcomeKind = "pushed"
	// EnvironmentBuildFileFault is an instruction of the file that failed: an exit code of a
	// step or an error of the frontend once the base was pulled.
	EnvironmentBuildFileFault EnvironmentBuildOutcomeKind = "file"
	// EnvironmentBuildPlatformFault is anything else: the base not pulled, the builder or the
	// registry unreachable. The build is assigned again.
	EnvironmentBuildPlatformFault EnvironmentBuildOutcomeKind = "platform"
)

// EnvironmentBuildReport is what a cluster reports of a build it was assigned at Attempt
// (POST /internal/clusters/{name}/builds/{key}). Digest, sha256:<hex>, goes with pushed only.
// LogTail is the end of the build's output, at most MaxEnvironmentLogBytes.
type EnvironmentBuildReport struct {
	Attempt int                         `json:"attempt"`
	Outcome EnvironmentBuildOutcomeKind `json:"outcome"`
	Digest  string                      `json:"digest,omitempty"`
	LogTail string                      `json:"log_tail,omitempty"`
}

// EnvironmentBuildLoaded is what a cluster reports of a build it was assigned at Attempt once its
// image is in the image store of its node Node as repository@Digest, while the push goes on
// (POST /internal/clusters/{name}/builds/{key}/loaded). A job placed on that node runs it from
// there before it is pushed.
type EnvironmentBuildLoaded struct {
	Attempt int    `json:"attempt"`
	Node    string `json:"node"`
	Digest  string `json:"digest"`
}

// MaxNodeNameBytes bounds a node's name, a DNS subdomain.
const MaxNodeNameBytes = 253

// MaxEnvironmentProgressBytes bounds the step of a build's progress report.
const MaxEnvironmentProgressBytes = 512

// EnvironmentBuildProgress is what a cluster reports of a build it was assigned at Attempt while
// it runs (POST /internal/clusters/{name}/builds/{key}/progress): Step, the build's latest step
// in one line of at most MaxEnvironmentProgressBytes, such as "[3/5] RUN apt-get install gcc:
// started". The job waiting for the build shows it.
type EnvironmentBuildProgress struct {
	Attempt int    `json:"attempt"`
	Step    string `json:"step"`
}
