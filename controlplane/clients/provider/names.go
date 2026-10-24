package provider

import (
	"fmt"
	"regexp"
)

// Every instance name starts with what the instance is for and whose it is: hl-job-<namespace>-
// for a job, hl-agent-<namespace>- for an agent, and a network of a job's hl-net-<namespace>-.
// The namespace keeps apart processes that share an account, and no full prefix is the prefix
// of another's or of anything else rented there.
const (
	jobPrefix     = "hl-job-"
	agentPrefix   = "hl-agent-"
	networkPrefix = "hl-net-"
)

// segmentPattern holds no dash: a name is read back from its ends, and a namespace with one
// could be the prefix of another's.
var segmentPattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// ValidateSegment is the rule for a namespace and for any other part of a name that a config
// file chooses. what names the setting in the error.
func ValidateSegment(what, value string) error {
	if !segmentPattern.MatchString(value) {
		return fmt.Errorf("%s %q must be 1 to 16 lowercase letters and digits", what, value)
	}
	return nil
}

// JobPrefix starts the name of every job instance of the namespace.
func JobPrefix(namespace string) string { return jobPrefix + namespace + "-" }

// AgentPrefix starts the name of every agent instance of the namespace.
func AgentPrefix(namespace string) string { return agentPrefix + namespace + "-" }

// NetworkPrefix starts the name of every network of the namespace's jobs.
func NetworkPrefix(namespace string) string { return networkPrefix + namespace + "-" }
