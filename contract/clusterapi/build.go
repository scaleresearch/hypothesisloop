package clusterapi

import "time"

// EnvironmentBuild is a build the control plane assigned to a cluster, as its reconcile answer
// lists it under builds. The cluster builds Dockerfile with the build argument BASE set to Base,
// copies PlatformTools from the image Tools into what it built when Tools is set, pushes the
// image by digest with no tag to Repository, and reports the outcome under Key and Attempt. It
// shares its build cache only with builds of the same CacheScope. A build that leaves the list
// is no longer the cluster's to run.
type EnvironmentBuild struct {
	Key        string `json:"key"`
	Attempt    int    `json:"attempt"`
	Base       string `json:"base"`
	Tools      string `json:"tools,omitempty"`
	Dockerfile string `json:"dockerfile"`
	Repository string `json:"repository"`
	CacheScope string `json:"cache_scope"`
}

// PlatformTools are the files of a job image a job needs from the platform: its client, and
// hl-clone, which fetches the job's commit. An environment gets them from the job image as its
// last layer.
var PlatformTools = []string{"/usr/local/bin/hl", "/usr/local/bin/hl-clone"}

// EnvironmentGitSecret is the id of the BuildKit secret a RUN of an environment file mounts to
// read the repository that declared it: one line of git's credential store, read-only.
const EnvironmentGitSecret = "git-credentials"

// RegistryLogin is a login to the registry host Registry, good until ExpiresAt. It is handed out
// to push one build (POST /internal/clusters/{name}/builds/{key}/credential) and, as registry_pull
// of the reconcile answer, to pull the environment images of the jobs the answer lists.
type RegistryLogin struct {
	Registry  string    `json:"registry"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expires_at"`
}

// EnvironmentBuildCredentialRequest asks for the push login, or the git credential, of the build
// of a key the cluster was assigned at Attempt.
type EnvironmentBuildCredentialRequest struct {
	Attempt int `json:"attempt"`
}

// EnvironmentGitCredential reads the repository that declared an environment, at URL, as git
// sends it, good until ExpiresAt, zero for one that does not run out. It is handed out to one
// build (POST /internal/clusters/{name}/builds/{key}/git-credential), which mounts it as the
// secret EnvironmentGitSecret, and is never written to disk or a layer. A 404 means the
// repository is read with no credential.
type EnvironmentGitCredential struct {
	URL       string    `json:"url"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expires_at"`
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

// MaxEnvironmentLogBytes bounds EnvironmentBuildReport.LogTail.
const MaxEnvironmentLogBytes = 4 << 10

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
// (POST /internal/clusters/{name}/builds/{key}/loaded).
type EnvironmentBuildLoaded struct {
	Attempt int    `json:"attempt"`
	Node    string `json:"node"`
	Digest  string `json:"digest"`
}

// MaxEnvironmentProgressBytes bounds EnvironmentBuildProgress.Step.
const MaxEnvironmentProgressBytes = 512

// EnvironmentBuildProgress is what a cluster reports of a build it was assigned at Attempt while
// it runs (POST /internal/clusters/{name}/builds/{key}/progress): Step, the build's latest step
// in one line of at most MaxEnvironmentProgressBytes.
type EnvironmentBuildProgress struct {
	Attempt int    `json:"attempt"`
	Step    string `json:"step"`
}
