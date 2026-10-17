package clusterexchange

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// maxAgentVersionLength bounds the version an agent says it runs, an image tag.
const maxAgentVersionLength = 128

// checkAgentVersion refuses a version that cannot be an image tag.
func checkAgentVersion(version string) error {
	if len(version) > maxAgentVersionLength || strings.ContainsFunc(version, unicode.IsControl) {
		return fmt.Errorf("%s is longer than %d characters or holds a control character", clusterapi.AgentVersionHeader, maxAgentVersionLength)
	}
	return nil
}
