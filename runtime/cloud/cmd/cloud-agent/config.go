package main

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/configfile"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

type config struct {
	// ClusterName is the name this account's instances are scheduled under.
	ClusterName string `yaml:"cluster_name"`
	// APIURL is polled by this process and handed to every job, so a rented instance must be
	// able to reach it.
	APIURL   string `yaml:"api_url"`
	JobImage string `yaml:"job_image"`
	// Namespace is part of every instance name, so agents sharing an account keep apart.
	Namespace string `yaml:"namespace"`
	// The loop intervals and how much of a job's output a status push relays. Each pass costs
	// calls to the vendor's API, so these are the operator's to set against its rate limit.
	ReconcileIntervalSeconds int `yaml:"reconcile_interval_seconds"`
	StatusIntervalSeconds    int `yaml:"status_interval_seconds"`
	LogTailLines             int `yaml:"log_tail_lines"`
	// How long a job may wait for a machine to rent, and a rented one to start, before the job
	// is given up here and the machine destroyed.
	OfferWaitSeconds int            `yaml:"offer_wait_seconds"`
	StartupSeconds   int            `yaml:"startup_seconds"`
	Provider         providerConfig `yaml:"provider"`
	Shapes           []shapeConfig  `yaml:"shapes"`

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
	var cfg config
	if err := configfile.Load(path, &cfg); err != nil {
		return nil, err
	}
	if cfg.ClusterName == "" || cfg.Provider.Name == "" || cfg.Provider.AccountID == "" {
		return nil, fmt.Errorf("config: cluster_name, provider.name and provider.account_id are required")
	}
	if cfg.OfferWaitSeconds <= 0 || cfg.StartupSeconds <= 0 {
		return nil, fmt.Errorf("config: offer_wait_seconds and startup_seconds must be positive")
	}
	if cfg.ReconcileIntervalSeconds <= 0 || cfg.StatusIntervalSeconds <= 0 {
		return nil, fmt.Errorf("config: reconcile_interval_seconds and status_interval_seconds must be positive")
	}
	for _, s := range cfg.Shapes {
		shape, err := s.shape()
		if err != nil {
			return nil, fmt.Errorf("config: shape %q: %w", s.Name, err)
		}
		cfg.shapes = append(cfg.shapes, shape)
	}
	if err := cfg.executor(nil).Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// executor is the executor's configuration over rented.
func (cfg *config) executor(rented provider.Provider) cloudexec.Config {
	return cloudexec.Config{
		ClusterID:    cfg.Provider.Name + "-" + cfg.Provider.AccountID,
		Namespace:    cfg.Namespace,
		APIURL:       cfg.APIURL,
		JobImage:     cfg.JobImage,
		LogTailLines: cfg.LogTailLines,
		OfferWait:    time.Duration(cfg.OfferWaitSeconds) * time.Second,
		Startup:      time.Duration(cfg.StartupSeconds) * time.Second,
		Shapes:       cfg.shapes,
		Provider:     rented,
	}
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
