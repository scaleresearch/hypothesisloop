// rented <example> <provider> | <example> <provider> <cluster_name> [<provider> <cluster_name>]...
// writes the sandbox or cloud_compute example of controlplane/settings as local development rents
// with it: each section names provider and keeps that provider's section alone, and each value
// there that .env holds as <SECTION>_<NAME>_<FIELD> (config.ProviderKey) is set from it, NAME the
// sandbox's provider or the cluster_name a cloud_compute entry is renamed to. Of cloud_compute it
// keeps one entry per pair, the example's entry on that provider, in the order the pairs are named.
// It refuses a provider this build has no adapter for, a placeholder left in a chosen section, and
// an account key .env does not hold.
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
	if len(os.Args) < 3 {
		fail(usage)
	}
	path := os.Args[1]
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
	args := os.Args[2:]
	if value.Kind != yaml.SequenceNode {
		if len(args) != 1 {
			fail(fmt.Sprintf("rented: %s lists no clusters to name", path))
		}
		known(args[0])
		choose(value, name, section, args[0], args[0])
		requireKey(section, args[0])
		encode(&document)
		return
	}
	if len(args)%2 != 0 {
		fail(fmt.Sprintf("rented: %s takes a provider and a cluster_name for each entry", path))
	}
	var entries []*yaml.Node
	named := map[string]bool{}
	for i := 0; i < len(args); i += 2 {
		provider, clusterName := args[i], args[i+1]
		known(provider)
		if named[clusterName] {
			fail(fmt.Sprintf("rented: cluster_name %s is named twice", clusterName))
		}
		named[clusterName] = true
		entry := entryOf(value, name, provider)
		choose(entry, name, section, provider, clusterName)
		rename(entry, name, clusterName)
		requireKey(section, clusterName)
		entries = append(entries, entry)
	}
	value.Content = entries
	encode(&document)
}

const usage = "usage: rented <example> <provider> | <example> <provider> <cluster_name> [<provider> <cluster_name>]..."

func known(provider string) {
	if !slices.Contains(providers.Names(), provider) {
		fail(fmt.Sprintf("rented: provider %q is not one this build rents on (%s)", provider, strings.Join(providers.Names(), ", ")))
	}
}

// entryOf is a copy of the example's entry whose provider is provider, or of its one entry when
// that holds a section of provider among others.
func entryOf(list *yaml.Node, name, provider string) *yaml.Node {
	for _, entry := range list.Content {
		for i := 0; i+1 < len(entry.Content); i += 2 {
			key, value := entry.Content[i].Value, entry.Content[i+1]
			if key == "provider" && value.Value == provider || key == provider && len(list.Content) == 1 {
				return deepCopy(entry)
			}
		}
	}
	fail(fmt.Sprintf("rented: %s has no entry on %s", name, provider))
	return nil
}

func deepCopy(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = deepCopy(child)
	}
	return &c
}

func requireKey(section, keyName string) {
	if key := config.AccountKey(section, keyName); os.Getenv(key.Name) == "" {
		fail(fmt.Sprintf("rented: .env does not set %s, %s", key.Name, key.For))
	}
}

func encode(document *yaml.Node) {
	encoder := yaml.NewEncoder(os.Stdout)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		fail(err.Error())
	}
}

// choose makes s the section of provider: its provider key names it, the sections of the other
// providers are dropped, and the values of provider's own are set from the environment.
func choose(s *yaml.Node, name, section, provider, keyName string) {
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
			fill(value, name, section, provider, keyName)
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
func fill(block *yaml.Node, name, section, provider, keyName string) {
	for i := 0; i+1 < len(block.Content); i += 2 {
		field, value := block.Content[i].Value, block.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			continue
		}
		key := config.ProviderKey(section, keyName, strings.ToUpper(field), "")
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
