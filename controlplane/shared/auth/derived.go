package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const derivedPrefix = "hls_"

// StaffScope is the scope of the platform's own operators, and of local development.
const StaffScope = "staff"

func JobScope(experimentID string) string { return "job:" + experimentID }

func AgentScope(platformExperimentID, agentID string) string {
	return "agent:" + platformExperimentID + "/" + agentID
}

func CoordinatorScope(platformExperimentID string) string {
	return "coordinator:" + platformExperimentID
}

func RuntimeScope(clusterName string) string { return "runtime:" + clusterName }

func LauncherScope(name string) string { return "launcher:" + name }

// BuildScope is the scope of the token one attempt of an environment build reads its repository
// with through the git proxy.
func BuildScope(cluster, key string, attempt int) string {
	return "build:" + cluster + "/" + key + "/" + strconv.Itoa(attempt)
}

var encoding = base64.RawURLEncoding

// Mint derives the token for a scope: hls_<scope>.<mac>, both parts base64url so the token is
// one HTTP token and can travel as a WebSocket subprotocol. Nothing is stored, so minting the
// same scope again returns the same token.
func Mint(signingSecret, scope string) (string, error) {
	if signingSecret == "" {
		return "", errors.New("auth: signing secret is empty")
	}
	if _, err := parseScope(scope); err != nil {
		return "", err
	}
	return derivedPrefix + encoding.EncodeToString([]byte(scope)) + "." + encoding.EncodeToString(mac(signingSecret, scope)), nil
}

func mac(signingSecret, scope string) []byte {
	h := hmac.New(sha256.New, []byte(signingSecret))
	h.Write([]byte(scope))
	return h.Sum(nil)
}

// verifyDerived returns the principal an hls_ token names. It checks the signature only: whether
// the thing named still exists is the authenticator's question.
func verifyDerived(signingSecret, token string) (Principal, error) {
	scopePart, macPart, ok := strings.Cut(strings.TrimPrefix(token, derivedPrefix), ".")
	if !ok {
		return Principal{}, errors.New("malformed token")
	}
	scope, err := encoding.DecodeString(scopePart)
	if err != nil {
		return Principal{}, errors.New("malformed token")
	}
	got, err := encoding.DecodeString(macPart)
	if err != nil {
		return Principal{}, errors.New("malformed token")
	}
	if !hmac.Equal(got, mac(signingSecret, string(scope))) {
		return Principal{}, errors.New("bad signature")
	}
	return parseScope(string(scope))
}

func parseScope(scope string) (Principal, error) {
	if scope == StaffScope {
		return Principal{Kind: KindStaff}, nil
	}
	kind, subject, ok := strings.Cut(scope, ":")
	if !ok || subject == "" {
		return Principal{}, fmt.Errorf("auth: unknown scope %q", scope)
	}
	switch Kind(kind) {
	case KindJob:
		return Principal{Kind: KindJob, ExperimentID: subject}, nil
	case KindAgent:
		pe, agent, ok := strings.Cut(subject, "/")
		if !ok || pe == "" || agent == "" {
			return Principal{}, fmt.Errorf("auth: agent scope must be agent:<platform experiment>/<agent>, got %q", scope)
		}
		return Principal{Kind: KindAgent, PlatformExperimentID: pe, AgentID: agent}, nil
	case KindCoordinator:
		return Principal{Kind: KindCoordinator, PlatformExperimentID: subject}, nil
	case KindRuntime:
		return Principal{Kind: KindRuntime, ClusterName: subject}, nil
	case KindLauncher:
		return Principal{Kind: KindLauncher, LauncherName: subject}, nil
	case KindBuild:
		parts := strings.Split(subject, "/")
		attempt, err := strconv.Atoi(parts[len(parts)-1])
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || err != nil || attempt < 1 || strconv.Itoa(attempt) != parts[2] {
			return Principal{}, fmt.Errorf("auth: build scope must be build:<cluster>/<key>/<attempt>, got %q", scope)
		}
		return Principal{Kind: KindBuild, Build: BuildRef{Cluster: parts[0], Key: parts[1], Attempt: attempt}}, nil
	default:
		return Principal{}, fmt.Errorf("auth: unknown scope %q", scope)
	}
}
