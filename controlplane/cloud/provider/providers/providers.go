package providers

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/e2b"
	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/fly"
	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/vast"
)

// byName is every vendor a binary can rent from, by the name a config file uses. A line is
// `"name": adapter(pkg.New)`, where pkg.New takes the package's own settings struct, the account's
// secret and the accelerator types the caller will ask it for.
var byName = map[string]constructor{
	"e2b":  adapter(e2b.New),
	"fly":  adapter(fly.New),
	"vast": adapter(vast.New),
}

type constructor func(settings yaml.Node, secret string, acceleratorTypes []string, log func(format string, args ...any)) (provider.Provider, error)

// adapter turns a package's New into a constructor that first decodes the provider's section of
// the config file into that package's settings. New is handed the accelerator types as well and
// refuses one it cannot rent, so a shape nothing could run on stops the process at startup.
func adapter[S any, P provider.Provider](construct func(S, string, []string, func(string, ...any)) (P, error)) constructor {
	return func(node yaml.Node, secret string, acceleratorTypes []string, log func(format string, args ...any)) (provider.Provider, error) {
		// A settings key with nothing under it is a section with no settings, the same as no
		// key at all, and the package's own check then names what is missing.
		if node.Tag == "!!null" {
			node = yaml.Node{}
		}
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		var settings S
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&settings); err != nil {
			return nil, fmt.Errorf("provider.settings: %w", err)
		}
		return construct(settings, secret, acceleratorTypes, log)
	}
}

// New builds the provider a config file names, from its own settings section and the account's
// secret, for the accelerator types the caller will ask it to rent, logging to log.
func New(name string, settings yaml.Node, secret string, acceleratorTypes []string, log func(format string, args ...any)) (provider.Provider, error) {
	construct, known := byName[name]
	if !known {
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("provider.name %q is not one this binary was built with (%s)", name, strings.Join(names, ", "))
	}
	return construct(settings, secret, acceleratorTypes, log)
}
