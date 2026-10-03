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

// callers says whose token runs a command. Every command's is stated in the table that lists it
// (subcommand.who): the listing a caller reads holds the commands that admit it, and the same
// value refuses the others before the command runs, as the platform would.
type callers int

const (
	everyone callers = iota
	// asAgent acts as an agent: an agent's token does, and a person or an API key naming one.
	asAgent
	// theAgentItself is an agent's own token and no other: nobody acts in its place.
	theAgentItself
	// ofTheRun runs a platform experiment: its coordinator, or a person or an API key of its org.
	ofTheRun
	// ofPeople is an org's own: no token bound to one platform experiment reaches an org.
	ofPeople
)

func (who callers) admits(kind callerKind) bool {
	switch who {
	case asAgent:
		return kind != callerCoordinator
	case theAgentItself:
		return kind == callerAgent
	case ofTheRun:
		return kind != callerAgent
	case ofPeople:
		return kind == callerOther
	}
	return true
}

// refusal is why a caller of kind, which who does not admit, may not run a command of who.
func (who callers) refusal(kind callerKind) error {
	whose := "an agent's"
	if kind == callerCoordinator {
		whose = "a coordinator's"
	}
	switch who {
	case asAgent:
		return errCoordinatorActsAsNoAgent
	case theAgentItself:
		if kind == callerCoordinator {
			return errCoordinatorActsAsNoAgent
		}
		return errors.New("an agent does this itself, with its own token, and HL_TOKEN is no agent's: the platform refuses it from anyone else")
	case ofTheRun:
		return errors.New("this is a command of the platform experiment's coordinator and of its org's people, and HL_TOKEN is " + whose + ". hl --help lists the commands this token runs")
	}
	return errors.New("this is a command of an org's people, and HL_TOKEN is " + whose + ", the token of one platform experiment, which reaches no org. hl capacity shows the clusters its jobs can run on, hl platform-experiments get its experiment, and hl --help the commands this token runs")
}

// errCoordinatorActsAsNoAgent refuses a coordinator every command that acts as an agent, and
// naming an agent in one it runs as itself: the platform refuses the same.
var errCoordinatorActsAsNoAgent = errors.New("this acts as an agent, and HL_TOKEN is a coordinator's, which is no agent and acts for none. " +
	"A coordinator writes the description (hl platform-experiments update) and the summary (hl platform-experiments summary), " +
	"adds agents (hl platform-experiments agents) and closes the run (hl platform-experiments close). " +
	"A hypothesis, a link or a comment it writes as itself, without --agent")

// recordOwner is the agent whose hypothesis, link, comment or verdict a command writes: the one
// --agent names, else the agent an agent's token is, else none. With none the
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
