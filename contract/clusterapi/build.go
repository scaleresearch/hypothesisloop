package clusterapi

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

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
// read the repository that declared it: a git config file (EnvironmentGitCredential.GitConfig),
// which git includes in EnvironmentGitRun.
const EnvironmentGitSecret = "git-credentials"

// EnvironmentGitRun is the line of an environment file that clones the repository it is of, the
// one an author is told to write: with the secret's config included, the repository's own address
// reaches it.
const EnvironmentGitRun = "RUN --mount=type=secret,id=" + EnvironmentGitSecret + " git -c include.path=/run/secrets/" + EnvironmentGitSecret + " clone <repository url> /tmp/src"

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

// EnvironmentGitCredential reads the repository that declared an environment through the
// control plane's git proxy at ProxyURL, with Token, a platform token that reads that repository
// alone while the build is assigned and building and does nothing else. RepositoryURL is the
// repository's own address, the one an environment file names. It is handed out to one build
// (POST /internal/clusters/{name}/builds/{key}/git-credential), which mounts GitConfig as the
// secret EnvironmentGitSecret, never written to disk or a layer. A 404 means the environment is
// of no repository.
type EnvironmentGitCredential struct {
	ProxyURL      string `json:"proxy_url"`
	RepositoryURL string `json:"repository_url"`
	Token         string `json:"token"`
}

// GitConfig is the git config EnvironmentGitRun includes: it rewrites RepositoryURL, with or
// without .git, to ProxyURL, and sends Token there alone.
func (c EnvironmentGitCredential) GitConfig() ([]byte, error) {
	for name, value := range map[string]string{"proxy_url": c.ProxyURL, "repository_url": c.RepositoryURL, "token": c.Token} {
		if value == "" || strings.ContainsAny(value, "\"\\\n\r") {
			return nil, fmt.Errorf("the git credential's %s is empty or holds a quote, a backslash or a line break", name)
		}
	}
	for name, value := range map[string]string{"proxy_url": c.ProxyURL, "repository_url": c.RepositoryURL} {
		address, err := url.Parse(value)
		if err != nil || address.Host == "" || address.Scheme != "http" && address.Scheme != "https" || address.User != nil || address.RawQuery != "" || address.Fragment != "" {
			return nil, fmt.Errorf("the git credential's %s %q is not an http(s) address of a host, without credentials, query or fragment", name, value)
		}
	}
	proxy := strings.TrimSuffix(strings.TrimSuffix(c.ProxyURL, "/"), ".git")
	repository := strings.TrimSuffix(strings.TrimSuffix(c.RepositoryURL, "/"), ".git")
	var b strings.Builder
	fmt.Fprintf(&b, "[url \"%s\"]\n\tinsteadOf = \"%s\"\n", proxy, repository)
	// git matches an http.<url> section only at a path boundary, so P does not cover P.git.
	for _, address := range []string{proxy, proxy + ".git"} {
		fmt.Fprintf(&b, "[http \"%s\"]\n\textraHeader = \"Authorization: Bearer %s\"\n", address, c.Token)
	}
	return []byte(b.String()), nil
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
