package api

import (
	"fmt"
	"strings"
	"unicode"
)

// AgentVersionHeader carries, on every call a cluster's agent makes, the version
// it runs: the image tag it was installed with (clusterapi.AgentVersionHeader).
const AgentVersionHeader = "X-Hypothesisloop-Agent-Version"

// maxAgentVersionLength bounds the version an agent says it runs, an image tag.
const maxAgentVersionLength = 128

// CheckAgentVersion refuses a version that cannot be an image tag.
func CheckAgentVersion(version string) error {
	if len(version) > maxAgentVersionLength || strings.ContainsFunc(version, unicode.IsControl) {
		return fmt.Errorf("%s is longer than %d characters or holds a control character", AgentVersionHeader, maxAgentVersionLength)
	}
	return nil
}
