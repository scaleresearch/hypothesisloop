package main

import (
	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/scope"
)

// callerKind is what kind of caller a token is, as far as hl can tell without asking.
type callerKind int

const (
	// callerOther is a person, an API key or staff: whoever it is, it may name any agent it reaches.
	callerOther callerKind = iota
	callerAgent
	callerCoordinator
)

// scopeOf reads what a token the platform derived says of its caller: its kind, its platform
// experiment and, for an agent, its agent. The platform alone verifies it.
func scopeOf(token string) (kind callerKind, platformExperiment, agent string) {
	raw, err := scope.Of(token)
	if err != nil {
		return callerOther, "", ""
	}
	named, err := scope.Parse(raw)
	if err != nil {
		return callerOther, "", ""
	}
	switch named.Kind {
	case scope.KindAgent:
		return callerAgent, named.PlatformExperimentID, named.AgentID
	case scope.KindCoordinator:
		return callerCoordinator, named.PlatformExperimentID, ""
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
