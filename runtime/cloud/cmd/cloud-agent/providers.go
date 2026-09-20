package main

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/vast"
)

// providers is every vendor this binary can rent from, by the name the config file uses. This is
// the one place a vendor is named outside its own package: adding one is its package and a line
// here, `"name": adapter(pkg.New)`, where pkg.New takes the package's own settings struct.
var providers = map[string]constructor{
	"vast": adapter(vast.New),
}

type constructor func(settings yaml.Node) (provider.Provider, error)

// adapter turns a package's New into a constructor that first decodes the provider's section of
// the config file into that package's settings.
func adapter[S any, P provider.Provider](construct func(S) (P, error)) constructor {
	return func(node yaml.Node) (provider.Provider, error) {
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		var settings S
		if err := decodeStrict(raw, &settings); err != nil {
			return nil, fmt.Errorf("provider.settings: %w", err)
		}
		return construct(settings)
	}
}

func newProvider(cfg providerConfig) (provider.Provider, error) {
	construct, known := providers[cfg.Name]
	if !known {
		names := make([]string, 0, len(providers))
		for name := range providers {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("config: provider.name %q is not one this binary was built with (%s)", cfg.Name, strings.Join(names, ", "))
	}
	return construct(cfg.Settings)
}
