package providers

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/e2b"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fly"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/runpod"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/vast"
)

// byName is every vendor a binary can rent from, by the name a config file uses. A line is
// `"name": adapter(pkg.New)`, where pkg.New takes the package's own settings struct, the account's
// secret and the accelerator types the caller will ask it for.
var byName = map[string]constructor{
	"e2b":    adapter(e2b.New),
	"fly":    adapter(fly.New),
	"runpod": adapter(runpod.New),
	"vast":   adapter(vast.New),
}

type constructor func(settings yaml.Node, secret string, acceleratorTypes []string, log func(format string, args ...any)) (provider.Provider, error)

// adapter turns a package's New into a constructor that first decodes the provider's section of
// the settings, the one under its name, into that package's settings. New is handed the accelerator types as well and
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
			return nil, fmt.Errorf("the section of the provider: %w", err)
		}
		return construct(settings, secret, acceleratorTypes, log)
	}
}

// Names is every provider this binary was built with, sorted.
func Names() []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// New builds the provider the settings name, from its own section of them and the account's
// secret, for the accelerator types the caller will ask it to rent, logging to log.
func New(name string, settings yaml.Node, secret string, acceleratorTypes []string, log func(format string, args ...any)) (provider.Provider, error) {
	construct, known := byName[name]
	if !known {
		return nil, fmt.Errorf("provider %q is not one this binary was built with (%s)", name, strings.Join(Names(), ", "))
	}
	return construct(settings, secret, acceleratorTypes, log)
}
