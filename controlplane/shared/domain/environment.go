package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

// EnvironmentFile is the file of an experiment repository that declares the environment its jobs
// run in: a Dockerfile built on the job's image, which the platform passes as BASE. Code is not in
// it: a job clones its commit at start, as without one.
const EnvironmentFile = ".hypothesisloop/Dockerfile"

// MaxEnvironmentFileBytes bounds an environment file.
const MaxEnvironmentFileBytes = 64 << 10

// MaxEnvironmentLogBytes bounds the end of a build's output that is kept and handed to the agent.
const MaxEnvironmentLogBytes = 4 << 10

// EnvironmentState is where the build of an environment is.
type EnvironmentState string

const (
	EnvironmentBuilding EnvironmentState = "building"
	EnvironmentReady    EnvironmentState = "ready"
	// EnvironmentFailed is a build that ended without an image, for the EnvironmentFault it
	// carries. The next submit that names it builds it again.
	EnvironmentFailed EnvironmentState = "failed"
)

// EnvironmentFault is whose fault a failed environment is.
type EnvironmentFault string

const (
	// EnvironmentFaultFile is an instruction of the file that failed.
	EnvironmentFaultFile EnvironmentFault = "file"
	// EnvironmentFaultPlatform is a build the platform did not finish on any of the
	// MaxEnvironmentAttempts assignments it made.
	EnvironmentFaultPlatform EnvironmentFault = "platform"
)

const (
	// MaxEnvironmentAttempts is how many times one build of an environment is assigned to a
	// cluster before it fails as the platform's fault.
	MaxEnvironmentAttempts = 3
	// MaxEnvironmentBuildsPerCluster is how many builds one cluster is assigned at once.
	MaxEnvironmentBuildsPerCluster = 2
	// EnvironmentBuildLease is how long a cluster holds an assigned build before it is assigned
	// again.
	EnvironmentBuildLease = 30 * time.Minute
	// EnvironmentBuildHold is how long a cluster that stopped reporting keeps the build it was
	// assigned. Its build goes on while the control plane is out of its reach, so a short gap
	// (a DNS or tunnel blip, an agent restart) neither cancels it nor spends an attempt on a cold
	// cluster; a cluster silent for longer is lost and the build is assigned again.
	EnvironmentBuildHold = 5 * time.Minute
	// MaxEnvironmentBuildTime is the longest the platform spends on one build before it fails it
	// as its own fault: every attempt, each for its lease.
	MaxEnvironmentBuildTime = MaxEnvironmentAttempts * EnvironmentBuildLease
)

// Environment is one image the platform builds for an org from an environment file on one base
// image, with the platform's tools from the image Tools added last, none when Tools is empty.
// Base and Tools are each repository@digest, and Key names the four, so the same file on the
// same images is built once per org however the images are tagged. A building
// one is assigned to ClusterName at AssignedAt for its Attempt-th time, or to none while
// ClusterName is empty. Image is the pushed build, repository@digest, and runs once State is
// ready. LogTail is the end of the output of a build that failed, and FailedLog that of the file's
// last failed build while a later submit builds the same file once more. Progress is the step its
// assigned cluster last reported of the attempt it builds, at ProgressAt. LoadedImage is the
// image of the last attempt that loaded one, repository@digest, once it is in the image store of
// node LoadedNode of LoadedCluster and while the build is not ready: a job pinned to that node runs
// it before the push ends, and still after a push that failed, while the build is tried again.
type Environment struct {
	Key           string
	OrgID         string
	Base          string
	Tools         string
	Dockerfile    string
	Image         string
	LoadedImage   string
	LoadedNode    string
	LoadedCluster string
	State         EnvironmentState
	Fault         EnvironmentFault
	LogTail       string
	FailedLog     string
	ClusterName   string
	Attempt       int
	AssignedAt    *time.Time
	Progress      string
	ProgressAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NodeHostnameLabel is the label that names one node, which a job is pinned to its node by.
const NodeHostnameLabel = "kubernetes.io/hostname"

// EnvironmentKey names the build of dockerfile on base with the tools of tools for orgID.
func EnvironmentKey(orgID, dockerfile, base, tools string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s%d:%s", len(orgID), orgID, len(base), base, len(dockerfile), dockerfile, len(tools), tools)))
	return hex.EncodeToString(sum[:])
}

// PlatformTools are the files of a job image a job needs from the platform: its client, and
// hl-clone, which fetches the job's commit. An environment built on a job image's base gets them
// from the job image as its last layer, so what the file installs stays cached across releases of
// the platform that change only them.
var PlatformTools = []string{"/usr/local/bin/hl", "/usr/local/bin/hl-clone"}

// EnvironmentBuildOutcome is what the cluster a build was assigned to reports of it.
type EnvironmentBuildOutcome struct {
	// Fault is empty for a build that pushed Image, repository@digest.
	Fault   EnvironmentFault
	Image   string
	LogTail string
}

const (
	// NotAdmittedEnvironmentBuilding holds a job whose commit declares an environment that is
	// being built.
	NotAdmittedEnvironmentBuilding = "environment_building"
	// HeldBaselineLate is the job.held reason of a job of an open platform experiment's baseline
	// that, started now, ends after its baseline_due_at.
	HeldBaselineLate = "baseline_late"
	// EvictionEnvironmentFailed ends a job whose environment file did not build.
	EvictionEnvironmentFailed EvictionReason = "environment_failed"
	// EvictionEnvironmentUnbuilt ends a job whose environment the platform did not build, no
	// fault of the file's. It costs nothing and is not queued again: the environment stays
	// failed until a submit names it, so the agent submits again.
	EvictionEnvironmentUnbuilt EvictionReason = "environment_unbuilt"
	// ReasonInvalidEnvironmentFile refuses an environment file that is not built on BASE.
	ReasonInvalidEnvironmentFile = "invalid_environment_file"
	// ReasonNoEnvironmentBuilder refuses a job whose commit declares an environment while no
	// cluster the org may use builds environments.
	ReasonNoEnvironmentBuilder = "no_environment_builder"
)

// ErrNoEnvironmentBuilder is an environment declared while no cluster the org may use builds.
var ErrNoEnvironmentBuilder = errors.New("no registered cluster the org may use builds environments")

// ImageUnresolvedError is an image an environment is built from whose registry did not name the
// digest of what it holds.
type ImageUnresolvedError struct {
	Image string
	Err   error
}

func (e *ImageUnresolvedError) Error() string {
	return "the registry of " + e.Image + " did not say which image it is: " + e.Err.Error()
}

func (e *ImageUnresolvedError) Unwrap() error { return e.Err }

// EnvironmentFileError is an environment file the platform does not build, with why.
type EnvironmentFileError struct{ Why string }

func (e *EnvironmentFileError) Error() string { return e.Why }

// ValidateEnvironmentFile accepts a file whose first two instructions are ARG BASE and FROM
// ${BASE}, so whatever it adds sits on the image the platform runs, and that reads nothing from a
// build context. Comments and blank lines may come before them.
func ValidateEnvironmentFile(dockerfile string) error {
	if len(dockerfile) > MaxEnvironmentFileBytes || !utf8.ValidString(dockerfile) {
		return &EnvironmentFileError{Why: fmt.Sprintf("%s must be UTF-8 text of at most %d bytes", EnvironmentFile, MaxEnvironmentFileBytes)}
	}
	var instructions [][]string
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		instructions = append(instructions, fields)
		if len(instructions) == 2 {
			break
		}
	}
	arg := len(instructions) > 0 && len(instructions[0]) == 2 && strings.EqualFold(instructions[0][0], "ARG") && instructions[0][1] == "BASE"
	from := len(instructions) > 1 && strings.EqualFold(instructions[1][0], "FROM") && len(instructions[1]) >= 2 &&
		(instructions[1][1] == "${BASE}" || instructions[1][1] == "$BASE") &&
		(len(instructions[1]) == 2 || len(instructions[1]) == 4 && strings.EqualFold(instructions[1][2], "AS"))
	if !arg || !from {
		return &EnvironmentFileError{Why: EnvironmentFile + " begins with the two lines ARG BASE and FROM ${BASE}: the platform passes as BASE the image the job names, without hl and hl-clone, which it adds last, and builds what follows on it"}
	}
	return refuseBuildContext(dockerfile)
}

// refuseBuildContext refuses a file that reads from a build context: the platform builds the
// file alone, so a COPY or ADD of a local path can only fail once it is built.
func refuseBuildContext(dockerfile string) error {
	parsed, err := parser.Parse(strings.NewReader(dockerfile))
	if err != nil {
		return &EnvironmentFileError{Why: EnvironmentFile + " does not parse: " + err.Error()}
	}
	for _, node := range parsed.AST.Children {
		instruction := strings.ToLower(node.Value)
		if instruction != "copy" && instruction != "add" {
			continue
		}
		if slices.ContainsFunc(node.Flags, func(flag string) bool { return strings.HasPrefix(flag, "--from") }) {
			continue
		}
		var args []string
		for next := node.Next; next != nil; next = next.Next {
			args = append(args, next.Value)
		}
		for _, source := range args[:max(len(args)-1, 0)] {
			if strings.HasPrefix(source, "<<") || instruction == "add" && remoteSource(source) {
				continue
			}
			return &EnvironmentFileError{Why: fmt.Sprintf("%s line %d, %s, reads %s from a build context the build does not have: the environment is built from this file alone, and a job gets the repository's code by its clone at start. Install what the code needs with RUN or COPY --from an image, and build the code itself in the job",
				EnvironmentFile, node.StartLine, strings.TrimSpace(node.Original), source)}
		}
	}
	return nil
}

func remoteSource(source string) bool {
	for _, prefix := range []string{"http://", "https://", "git@", "git://"} {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return false
}
