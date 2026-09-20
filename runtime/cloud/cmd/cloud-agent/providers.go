package main

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/vast"
)

// providers is every vendor this binary can rent from, by the name the config file uses. This is
// the one place a vendor is named outside its own package: adding one is its package and a line
// here, `"name": adapter(pkg.New)`, where pkg.New takes the package's own settings struct and the
// accelerator types the configured shapes will ask it for.
var providers = map[string]constructor{
	"vast": adapter(vast.New),
}

type constructor func(settings yaml.Node, acceleratorTypes []string) (provider.Provider, error)

// adapter turns a package's New into a constructor that first decodes the provider's section of
// the config file into that package's settings. New is handed the accelerator types as well and
// refuses one it cannot rent, so a shape no job could run on stops the process at startup.
func adapter[S any, P provider.Provider](construct func(S, []string) (P, error)) constructor {
	return func(node yaml.Node, acceleratorTypes []string) (provider.Provider, error) {
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		var settings S
		if err := decodeStrict(raw, &settings); err != nil {
			return nil, fmt.Errorf("provider.settings: %w", err)
		}
		return construct(settings, acceleratorTypes)
	}
}

// newProvider builds the configured provider for the accelerator types of the shapes it will be
// asked to rent.
func newProvider(cfg providerConfig, shapes []cloudexec.Shape) (provider.Provider, error) {
	construct, known := providers[cfg.Name]
	if !known {
		names := make([]string, 0, len(providers))
		for name := range providers {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("config: provider.name %q is not one this binary was built with (%s)", cfg.Name, strings.Join(names, ", "))
	}
	var acceleratorTypes []string
	for _, shape := range shapes {
		if shape.Accelerators > 0 {
			acceleratorTypes = append(acceleratorTypes, shape.AcceleratorType)
		}
	}
	return construct(cfg.Settings, acceleratorTypes)
}
