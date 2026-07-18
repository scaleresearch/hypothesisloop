package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ClusterEntry describes one target cluster the control plane can admit
// experiments onto.
type ClusterEntry struct {
	Name           string `yaml:"name"`
	KubeconfigPath string `yaml:"kubeconfig_path"`
	Context        string `yaml:"context"`
}

// ClustersConfig is the top-level shape of a clusters.yaml file.
type ClustersConfig struct {
	Clusters []ClusterEntry `yaml:"clusters"`
}

// LoadClusters reads a clusters.yaml-shaped file and returns its cluster entries.
// If path is empty or the file does not exist, it returns (nil, nil) so callers can
// fall back to single-cluster behavior built from env vars (preserves today's behavior
// exactly).
func LoadClusters(path string) ([]ClusterEntry, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cc ClustersConfig
	if err := yaml.Unmarshal(data, &cc); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	for i, c := range cc.Clusters {
		if c.Name == "" {
			return nil, fmt.Errorf("config: %s: cluster entry %d missing name", path, i)
		}
	}
	return cc.Clusters, nil
}
