package main

import (
	"encoding/base64"
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

// scopeOf reads what a token the platform derived says of its caller (hls_<scope>.<mac>, the
// scope being base64url of "agent:<pe>/<agent>" or "coordinator:<pe>"): its kind, its platform
// experiment and, for an agent, its agent. The platform alone verifies it.
func scopeOf(token string) (kind callerKind, platformExperiment, agent string) {
	scopePart, _, ok := strings.Cut(strings.TrimPrefix(token, "hls_"), ".")
	if !ok || !strings.HasPrefix(token, "hls_") {
		return callerOther, "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(scopePart)
	if err != nil {
		return callerOther, "", ""
	}
	if rest, ok := strings.CutPrefix(string(raw), "agent:"); ok {
		if pe, agent, ok := strings.Cut(rest, "/"); ok {
			return callerAgent, pe, agent
		}
	}
	if pe, ok := strings.CutPrefix(string(raw), "coordinator:"); ok {
		return callerCoordinator, pe, ""
	}
	return callerOther, "", ""
}

func kindOf(token string) callerKind {
	kind, _, _ := scopeOf(token)
	return kind
}

// experimentOf is the platform experiment a command works in: the one its flag names, else the
// one an agent's or a coordinator's token is bound to. Nothing else names one: a variable left in
// an environment once named another's.
func experimentOf(c *apiclient.Client, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	_, pe, _ := scopeOf(c.Token)
	return pe
}

// agentOf is the agent a command is about: the one its flag names, else the agent an agent's
// token is, else none.
func agentOf(c *apiclient.Client, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	_, _, agent := scopeOf(c.Token)
	return agent
}

// readsEveryAgent says whether a read that defaults to the caller's own agent covers the whole
// platform experiment instead: a coordinator's does, since it has no agent of its own.
func readsEveryAgent(token, agentFlag string) bool {
	return kindOf(token) == callerCoordinator && agentFlag == ""
}
