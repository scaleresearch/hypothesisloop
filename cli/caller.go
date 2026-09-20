package main

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

// callerKind is what kind of caller a token is, as far as hl can tell without asking.
type callerKind int

const (
	// callerOther is a person, an API key or staff: whoever it is, it may name any agent it reaches.
	callerOther callerKind = iota
	callerAgent
	callerCoordinator
)

// kindOf reads the kind from a token the platform derived (hls_<scope>.<mac>, the scope being
// base64url of "agent:<pe>/<agent>" or "coordinator:<pe>"). The platform alone verifies it.
func kindOf(token string) callerKind {
	scopePart, _, ok := strings.Cut(strings.TrimPrefix(token, "hls_"), ".")
	if !ok || !strings.HasPrefix(token, "hls_") {
		return callerOther
	}
	scope, err := base64.RawURLEncoding.DecodeString(scopePart)
	if err != nil {
		return callerOther
	}
	switch {
	case strings.HasPrefix(string(scope), "agent:"):
		return callerAgent
	case strings.HasPrefix(string(scope), "coordinator:"):
		return callerCoordinator
	}
	return callerOther
}

// errCoordinatorWritesAsNoAgent refuses a command that acts as an agent to a coordinator that
// named none: $AGENT_ID is not read for it, since a coordinator is no agent.
var errCoordinatorWritesAsNoAgent = errors.New("this acts as an agent, and HL_TOKEN is a coordinator's, which is no agent. " +
	"A coordinator writes the description (hl platform-experiments update) and the summary (hl platform-experiments summary); " +
	"--agent A acts for one agent of the platform experiment")

// writingAgent is the agent a command writes as: one branch per kind of caller. An agent and
// anyone else default to $AGENT_ID. A coordinator has no agent id of its own, so it writes for
// an agent only when --agent names one. An empty answer with no error means none was given.
func writingAgent(c *apiclient.Client, flagValue string) (string, error) {
	switch kindOf(c.Token) {
	case callerCoordinator:
		if flagValue == "" {
			return "", errCoordinatorWritesAsNoAgent
		}
		return flagValue, nil
	case callerAgent:
		return orEnv(flagValue, agentEnv), nil
	default:
		return orEnv(flagValue, agentEnv), nil
	}
}

// readsEveryAgent says whether a read that defaults to the caller's own agent covers the whole
// platform experiment instead: a coordinator's does, since it has no agent of its own.
func readsEveryAgent(token, agentFlag string) bool {
	return kindOf(token) == callerCoordinator && agentFlag == ""
}
