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
	"time"
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
)

// Staff is the scope of the platform's own operators in local development, until expiresAt.
func Staff(expiresAt time.Time) string {
	return string(KindStaff) + ":" + strconv.FormatInt(expiresAt.Unix(), 10)
}

func Job(experimentID string) string { return string(KindJob) + ":" + experimentID }

func Agent(platformExperimentID, agentID string) string {
	return string(KindAgent) + ":" + platformExperimentID + "/" + agentID
}

func Coordinator(platformExperimentID string) string {
	return string(KindCoordinator) + ":" + platformExperimentID
}

// Runtime names the registration of a cluster, so a token minted for a name that was removed and
// registered again, by any org, names a registration that no longer stands.
func Runtime(clusterName string, registeredAt time.Time) string {
	return string(KindRuntime) + ":" + clusterName + "/" + strconv.FormatInt(registeredAt.UnixMicro(), 10)
}

// Scope is what a scope names. Each kind sets the fields that name its subject.
type Scope struct {
	Kind                 Kind
	ExperimentID         string
	PlatformExperimentID string
	AgentID              string
	ClusterName          string
	ClusterRegisteredAt  time.Time
	ExpiresAt            time.Time
}

// Parse reads what scope names, refusing one of no kind or of a malformed subject.
func Parse(scope string) (Scope, error) {
	kind, subject, ok := strings.Cut(scope, ":")
	if !ok || subject == "" {
		return Scope{}, fmt.Errorf("unknown scope %q", scope)
	}
	switch Kind(kind) {
	case KindStaff:
		seconds, err := strconv.ParseInt(subject, 10, 64)
		if err != nil {
			return Scope{}, fmt.Errorf("staff scope must be staff:<expires at, unix seconds>, got %q", scope)
		}
		return Scope{Kind: KindStaff, ExpiresAt: time.Unix(seconds, 0)}, nil
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
		name, registered, ok := strings.Cut(subject, "/")
		micros, err := strconv.ParseInt(registered, 10, 64)
		if !ok || name == "" || err != nil {
			return Scope{}, fmt.Errorf("runtime scope must be runtime:<cluster>/<registered at, unix microseconds>, got %q", scope)
		}
		return Scope{Kind: KindRuntime, ClusterName: name, ClusterRegisteredAt: time.UnixMicro(micros)}, nil
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
