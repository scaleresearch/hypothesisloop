package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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
	// EnvironmentFailed is a file a Dockerfile instruction of which failed. It is final for that
	// file on that base.
	EnvironmentFailed EnvironmentState = "failed"
	// EnvironmentAbandoned is one the platform failed to build on every attempt it allows, no
	// fault of the file's. The next submit or declaration that names it builds it again.
	EnvironmentAbandoned EnvironmentState = "abandoned"
)

// Environment is one image the platform builds from an environment file on one base image. Key
// names the pair, so the same file on the same base is built once. Image is where the build is
// pushed, and runs once State is ready. LogTail is the end of a failed build's output. Attempts
// counts the builds the platform failed since it was last made building.
type Environment struct {
	Key        string
	Base       string
	Dockerfile string
	Image      string
	State      EnvironmentState
	LogTail    string
	Attempts   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// EnvironmentKey names the build of dockerfile on base.
func EnvironmentKey(dockerfile, base string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s", len(base), base, len(dockerfile), dockerfile)))
	return hex.EncodeToString(sum[:])
}

const (
	// NotAdmittedEnvironmentBuilding holds a job whose commit declares an environment that is
	// being built.
	NotAdmittedEnvironmentBuilding = "environment_building"
	// EvictionEnvironmentFailed ends a job whose environment file did not build.
	EvictionEnvironmentFailed EvictionReason = "environment_failed"
	// ReasonInvalidEnvironmentFile refuses an environment file that is not built on BASE.
	ReasonInvalidEnvironmentFile = "invalid_environment_file"
)

// EnvironmentFileError is an environment file the platform does not build, with why.
type EnvironmentFileError struct{ Why string }

func (e *EnvironmentFileError) Error() string { return e.Why }

// ValidateEnvironmentFile accepts a file whose first two instructions are ARG BASE and FROM
// ${BASE}, so whatever it adds sits on the image the platform runs (hl inside). Comments and
// blank lines may come before them.
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
		return &EnvironmentFileError{Why: EnvironmentFile + " begins with the two lines ARG BASE and FROM ${BASE}: the platform passes as BASE the image the job names, and builds what follows on it"}
	}
	return nil
}
