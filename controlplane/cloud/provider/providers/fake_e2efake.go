//go:build e2efake

package providers

import (
	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/fake"
)

// A binary built with -tags e2efake can be configured with the in-memory provider, so that an
// end-to-end test runs a real process against a real control plane and rents nothing. No image
// is built with the tag.
func init() {
	byName["fake"] = func(yaml.Node, string, []string, func(string, ...any)) (provider.Provider, error) {
		return fake.New(), nil
	}
}
