// rented <example> <provider> [<cluster_name>] writes the sandbox or cloud_compute example of
// controlplane/settings as local development rents with it: each section names provider and keeps
// that provider's section alone, and each value there that .env holds as
// <SECTION>_<PROVIDER>_<FIELD> is set from it. The cloud_compute example takes the cluster_name
// its entry is renamed to. It refuses a provider this build has no adapter for, a placeholder left
// in the chosen section, and an account key .env does not hold.
package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/providers"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

func main() {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		fail("usage: rented <example> <provider> [<cluster_name>]")
	}
	path, provider := os.Args[1], os.Args[2]
	if !slices.Contains(providers.Names(), provider) {
		fail(fmt.Sprintf("rented: provider %q is not one this build rents on (%s)", provider, strings.Join(providers.Names(), ", ")))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fail(err.Error())
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		fail(fmt.Sprintf("rented: %s: %v", path, err))
	}
	top := document.Content[0]
	if top.Kind != yaml.MappingNode || len(top.Content) != 2 {
		fail(fmt.Sprintf("rented: %s holds more or less than one section", path))
	}
	name, value := top.Content[0].Value, top.Content[1]
	section := strings.ToUpper(name)
	sections, account := []*yaml.Node{value}, provider
	switch {
	case value.Kind == yaml.SequenceNode && (len(os.Args) != 4 || len(value.Content) != 1):
		fail(fmt.Sprintf("rented: %s takes one entry and the cluster_name to give it", path))
	case value.Kind == yaml.SequenceNode:
		sections, account = value.Content, os.Args[3]
	case len(os.Args) == 4:
		fail(fmt.Sprintf("rented: %s lists no clusters to name", path))
	}
	for _, s := range sections {
		choose(s, name, section, provider)
	}
	if value.Kind == yaml.SequenceNode {
		rename(value.Content[0], name, account)
	}
	if key := config.AccountKey(section, account); os.Getenv(key.Name) == "" {
		fail(fmt.Sprintf("rented: .env does not set %s, %s", key.Name, key.For))
	}
	encoder := yaml.NewEncoder(os.Stdout)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		fail(err.Error())
	}
}

// choose makes s the section of provider: its provider key names it, the sections of the other
// providers are dropped, and the values of provider's own are set from the environment.
func choose(s *yaml.Node, name, section, provider string) {
	if s.Kind != yaml.MappingNode {
		fail(fmt.Sprintf("rented: %s is not a mapping", name))
	}
	kept, named, found := []*yaml.Node{}, false, false
	for i := 0; i < len(s.Content); i += 2 {
		key, value := s.Content[i], s.Content[i+1]
		switch {
		case key.Value == "provider":
			value.Value, value.Style, named = provider, 0, true
		case key.Value == provider:
			fill(value, name, section, provider)
			found = true
		case slices.Contains(providers.Names(), key.Value):
			continue
		}
		kept = append(kept, key, value)
	}
	if !named || !found {
		fail(fmt.Sprintf("rented: %s has no provider key or no section of %s", name, provider))
	}
	s.Content = kept
}

// rename sets the cluster_name of an entry.
func rename(entry *yaml.Node, name, clusterName string) {
	for i := 0; i+1 < len(entry.Content); i += 2 {
		if entry.Content[i].Value == "cluster_name" {
			entry.Content[i+1].Value, entry.Content[i+1].Style = clusterName, 0
			return
		}
	}
	fail(fmt.Sprintf("rented: %s has no cluster_name", name))
}

// fill sets each scalar value of a provider's section that the environment holds, and refuses one
// left a placeholder.
func fill(block *yaml.Node, name, section, provider string) {
	for i := 0; i+1 < len(block.Content); i += 2 {
		field, value := block.Content[i].Value, block.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			continue
		}
		key := config.ProviderKey(section, provider, strings.ToUpper(field), "")
		// The example's own tag is kept, so a value .env sets is read as the example's value is.
		if set := os.Getenv(key.Name); set != "" {
			value.Value, value.Style = set, 0
		}
		if strings.Contains(value.Value, config.Placeholder) {
			fail(fmt.Sprintf("rented: .env does not set %s, the %s.%s.%s of this deployment", key.Name, name, provider, field))
		}
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
