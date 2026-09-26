package main

import (
	"encoding/base64"
	"errors"
	"os"
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
// one an agent's or a coordinator's token is bound to, else $PLATFORM_EXPERIMENT_ID.
func experimentOf(c *apiclient.Client, flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if _, pe, _ := scopeOf(c.Token); pe != "" {
		return pe
	}
	return os.Getenv(platformExperimentEnv)
}

// agentOf is the agent a command is about: the one its flag names, else the agent an agent's
// token is, else $AGENT_ID. A coordinator is no agent, so $AGENT_ID is not read for it.
func agentOf(c *apiclient.Client, flagValue string) string {
	kind, _, agent := scopeOf(c.Token)
	switch {
	case flagValue != "":
		return flagValue
	case kind == callerAgent:
		return agent
	case kind == callerCoordinator:
		return ""
	}
	return os.Getenv(agentEnv)
}

// errCoordinatorActsAsNoAgent refuses a coordinator every command that acts as an agent, whether
// or not it names one: the platform refuses the same, and $AGENT_ID is not read for it.
var errCoordinatorActsAsNoAgent = errors.New("this acts as an agent, and HL_TOKEN is a coordinator's, which is no agent and acts for none. " +
	"A coordinator writes the description (hl platform-experiments update) and the summary (hl platform-experiments summary), " +
	"adds agents (hl platform-experiments agents) and closes the run (hl platform-experiments close). " +
	"A hypothesis, a link or a comment it writes as itself, without --agent")

// writingAgent is the agent a command spends as (see agentOf). An empty answer with no error
// means none was given.
func writingAgent(c *apiclient.Client, flagValue string) (string, error) {
	if kindOf(c.Token) == callerCoordinator {
		return "", errCoordinatorActsAsNoAgent
	}
	return agentOf(c, flagValue), nil
}

// recordOwner is the agent whose hypothesis, link, comment or verdict a command writes: the one
// --agent names, else the agent an agent's token is, else $AGENT_ID, else none. With none the
// record is the caller's own and no agent's. A coordinator writes as itself only: it acts for no
// agent, so naming one is refused.
func recordOwner(c *apiclient.Client, flagValue string) (string, error) {
	if kindOf(c.Token) == callerCoordinator && flagValue != "" {
		return "", errCoordinatorActsAsNoAgent
	}
	return agentOf(c, flagValue), nil
}

// readsEveryAgent says whether a read that defaults to the caller's own agent covers the whole
// platform experiment instead: a coordinator's does, since it has no agent of its own.
func readsEveryAgent(token, agentFlag string) bool {
	return kindOf(token) == callerCoordinator && agentFlag == ""
}
