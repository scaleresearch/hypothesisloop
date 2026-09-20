package main

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
)

type config struct {
	// ClusterName is the name this account's instances are scheduled under.
	ClusterName string `yaml:"cluster_name"`
	// APIURL is polled by this process and handed to every job, so a rented instance must be
	// able to reach it.
	APIURL   string         `yaml:"api_url"`
	JobImage string         `yaml:"job_image"`
	Provider providerConfig `yaml:"provider"`
	Shapes   []shapeConfig  `yaml:"shapes"`

	shapes []cloudexec.Shape
}

type providerConfig struct {
	Name string `yaml:"name"`
	// AccountID tells two accounts on one provider apart. With the name it is the cluster's id.
	AccountID string `yaml:"account_id"`
	// Settings is the provider's own section. Only its adapter reads it.
	Settings yaml.Node `yaml:"settings"`
}

type shapeConfig struct {
	Name            string `yaml:"name"`
	AcceleratorType string `yaml:"accelerator_type"`
	Accelerators    int    `yaml:"accelerators"`
	CPU             string `yaml:"cpu"`
	Memory          string `yaml:"memory"`
	Storage         string `yaml:"storage"`
	MaxInstances    int    `yaml:"max_instances"`
}

func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg config
	if err := decodeStrict(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.ClusterName == "" || cfg.APIURL == "" || cfg.JobImage == "" || cfg.Provider.Name == "" || cfg.Provider.AccountID == "" {
		return nil, fmt.Errorf("config: cluster_name, api_url, job_image, provider.name and provider.account_id are required")
	}
	for _, s := range cfg.Shapes {
		shape, err := s.shape()
		if err != nil {
			return nil, fmt.Errorf("config: shape %q: %w", s.Name, err)
		}
		cfg.shapes = append(cfg.shapes, shape)
	}
	return &cfg, nil
}

// shape parses the quantities, written as a job writes its own ("8", "32Gi"). Whether the shape
// makes sense is cloudexec's to say.
func (s shapeConfig) shape() (cloudexec.Shape, error) {
	cpu, err := resource.ParseQuantity(s.CPU)
	if err != nil {
		return cloudexec.Shape{}, fmt.Errorf("cpu %q: %w", s.CPU, err)
	}
	memory, err := resource.ParseQuantity(s.Memory)
	if err != nil {
		return cloudexec.Shape{}, fmt.Errorf("memory %q: %w", s.Memory, err)
	}
	storage, err := resource.ParseQuantity(s.Storage)
	if err != nil {
		return cloudexec.Shape{}, fmt.Errorf("storage %q: %w", s.Storage, err)
	}
	return cloudexec.Shape{
		Name:            s.Name,
		AcceleratorType: s.AcceleratorType,
		Accelerators:    s.Accelerators,
		CPUMillis:       cpu.MilliValue(),
		MemoryBytes:     memory.Value(),
		StorageBytes:    storage.Value(),
		MaxInstances:    s.MaxInstances,
	}, nil
}

// decodeStrict refuses a key the target does not have: a misspelled setting is an error at
// startup instead of a default nobody chose.
func decodeStrict(data []byte, into any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(into)
}
