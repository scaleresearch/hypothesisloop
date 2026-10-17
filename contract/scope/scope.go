// Package scope is the format of the tokens the platform derives: hls_<scope>.<mac>, both parts
// base64url so a token is one HTTP token and can travel as a WebSocket subprotocol. What a scope
// names can be read from a token by anyone; only the platform verifies its mac.
package scope

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Prefix begins every token the platform derives.
const Prefix = "hls_"

// Kind is what a scope is of.
type Kind string

const (
	KindStaff       Kind = "staff"
	KindJob         Kind = "job"
	KindAgent       Kind = "agent"
	KindCoordinator Kind = "coordinator"
	KindRuntime     Kind = "runtime"
	KindBuild       Kind = "build"
)

// Staff is the scope of the platform's own operators, and of local development.
const Staff = string(KindStaff)

func Job(experimentID string) string { return string(KindJob) + ":" + experimentID }

func Agent(platformExperimentID, agentID string) string {
	return string(KindAgent) + ":" + platformExperimentID + "/" + agentID
}

func Coordinator(platformExperimentID string) string {
	return string(KindCoordinator) + ":" + platformExperimentID
}

func Runtime(clusterName string) string { return string(KindRuntime) + ":" + clusterName }

// Build is the scope of the token one attempt of an environment build reads its repository with
// through the git proxy.
func Build(cluster, key string, attempt int) string {
	return string(KindBuild) + ":" + cluster + "/" + key + "/" + strconv.Itoa(attempt)
}

// Scope is what a scope names. Each kind sets the fields that name its subject.
type Scope struct {
	Kind                 Kind
	ExperimentID         string
	PlatformExperimentID string
	AgentID              string
	ClusterName          string
	Build                BuildRef
}

// BuildRef names one attempt of an environment build on the cluster it was assigned to.
type BuildRef struct {
	Cluster string
	Key     string
	Attempt int
}

// Parse reads what scope names, refusing one of no kind or of a malformed subject.
func Parse(scope string) (Scope, error) {
	if scope == Staff {
		return Scope{Kind: KindStaff}, nil
	}
	kind, subject, ok := strings.Cut(scope, ":")
	if !ok || subject == "" {
		return Scope{}, fmt.Errorf("unknown scope %q", scope)
	}
	switch Kind(kind) {
	case KindJob:
		return Scope{Kind: KindJob, ExperimentID: subject}, nil
	case KindAgent:
		pe, agent, ok := strings.Cut(subject, "/")
		if !ok || pe == "" || agent == "" {
			return Scope{}, fmt.Errorf("agent scope must be agent:<platform experiment>/<agent>, got %q", scope)
		}
		return Scope{Kind: KindAgent, PlatformExperimentID: pe, AgentID: agent}, nil
	case KindCoordinator:
		return Scope{Kind: KindCoordinator, PlatformExperimentID: subject}, nil
	case KindRuntime:
		return Scope{Kind: KindRuntime, ClusterName: subject}, nil
	case KindBuild:
		parts := strings.Split(subject, "/")
		attempt, err := strconv.Atoi(parts[len(parts)-1])
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || err != nil || attempt < 1 || strconv.Itoa(attempt) != parts[2] {
			return Scope{}, fmt.Errorf("build scope must be build:<cluster>/<key>/<attempt>, got %q", scope)
		}
		return Scope{Kind: KindBuild, Build: BuildRef{Cluster: parts[0], Key: parts[1], Attempt: attempt}}, nil
	default:
		return Scope{}, fmt.Errorf("unknown scope %q", scope)
	}
}

var encoding = base64.RawURLEncoding

// Token is the token of scope with mac, the platform's signature of it.
func Token(scope string, mac []byte) string {
	return Prefix + encoding.EncodeToString([]byte(scope)) + "." + encoding.EncodeToString(mac)
}

// Of is the scope a derived token names, unverified: only the platform checks its mac.
func Of(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, Prefix)
	if !ok {
		return "", errors.New("not a derived token")
	}
	scopePart, _, ok := strings.Cut(rest, ".")
	if !ok {
		return "", errors.New("malformed token")
	}
	raw, err := encoding.DecodeString(scopePart)
	if err != nil {
		return "", errors.New("malformed token")
	}
	return string(raw), nil
}
