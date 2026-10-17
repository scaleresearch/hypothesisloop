package clusteragent

import (
	_ "embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

//go:embed Chart.yaml
var chartFile []byte

var chartVersion = func() string {
	var chart struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(chartFile, &chart); err != nil || chart.Version == "" {
		panic(fmt.Sprintf("deploy/charts/hypothesisloop-cluster-agent/Chart.yaml names no version: %v", err))
	}
	return chart.Version
}()

// Version is the version make publish pushes this chart under beside the images tagged tag: the
// Chart.yaml version with tag as build metadata.
func Version(tag string) string {
	return chartVersion + "+" + tag
}
