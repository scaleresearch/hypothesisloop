package apiclient

import (
	"github.com/scaleresearch/hypothesisloop/contract/scope"
)

// CallerKind is what kind of caller a token is, as far as hl can tell without asking.
type CallerKind int

const (
	// CallerOther is a person, an API key or staff: whoever it is, it may name any agent it reaches.
	CallerOther CallerKind = iota
	CallerAgent
	CallerCoordinator
)

// ScopeOf reads what a token the platform derived says of its caller: its kind, its platform
// experiment and, for an agent, its agent. The platform alone verifies it.
func ScopeOf(token string) (kind CallerKind, platformExperiment, agent string) {
	raw, err := scope.Of(token)
	if err != nil {
		return CallerOther, "", ""
	}
	named, err := scope.Parse(raw)
	if err != nil {
		return CallerOther, "", ""
	}
	switch named.Kind {
	case scope.KindAgent:
		return CallerAgent, named.PlatformExperimentID, named.AgentID
	case scope.KindCoordinator:
		return CallerCoordinator, named.PlatformExperimentID, ""
	}
	return CallerOther, "", ""
}

func KindOf(token string) CallerKind {
	kind, _, _ := ScopeOf(token)
	return kind
}

// Experiment is the platform experiment a command works in: the one its flag names, else the
// one an agent's or a coordinator's token is bound to. Nothing else names one: a variable left in
// an environment once named another's.
func (c *Client) Experiment(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	_, pe, _ := ScopeOf(c.Token)
	return pe
}

// Agent is the agent a command is about: the one its flag names, else the agent an agent's
// token is, else none.
func (c *Client) Agent(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	_, _, agent := ScopeOf(c.Token)
	return agent
}

// ReadsEveryAgent says whether a read that defaults to the caller's own agent covers the whole
// platform experiment instead: a coordinator's does, since it has no agent of its own.
func ReadsEveryAgent(token, agentFlag string) bool {
	return KindOf(token) == CallerCoordinator && agentFlag == ""
}
