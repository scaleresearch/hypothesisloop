package main

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/configfile"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

type config struct {
	// LauncherName is the name this launcher is registered under with the control plane, the name
	// it asks for its list under, and part of every instance name.
	LauncherName string `yaml:"launcher_name"`
	// APIURL is where this process reaches the control plane. The address agents reach it at is
	// the control plane's own setting and arrives in each instance's environment.
	APIURL string `yaml:"api_url"`
	// Each pass costs the provider one list call and the control plane one request.
	ReconcileIntervalSeconds int `yaml:"reconcile_interval_seconds"`
	// CreatePauseMilliseconds is waited between two creates of one pass. 0 is no pause, and the
	// key must be present either way.
	CreatePauseMilliseconds *int `yaml:"create_pause_milliseconds"`
	// WarmPoolSize is how many warm instances the launcher keeps ready to start agents in. Left
	// out, it is defaultWarmPool on a provider that can warm instances and none on another.
	WarmPoolSize *int           `yaml:"warm_pool_size"`
	Provider     providerConfig `yaml:"provider"`
	Instance     instanceConfig `yaml:"instance"`

	size agentlaunch.Size
}

type providerConfig struct {
	Name string `yaml:"name"`
	// Settings is the provider's own section. Only its adapter reads it.
	Settings yaml.Node `yaml:"settings"`
}

// instanceConfig is what every agent instance runs and how large it is rented.
type instanceConfig struct {
	// Command is the whole argv, in place of the image's entrypoint.
	Command []string `yaml:"command"`
	CPU     string   `yaml:"cpu"`
	Memory  string   `yaml:"memory"`
	Storage string   `yaml:"storage"`
}

func loadConfig(path string) (*config, error) {
	var cfg config
	if err := configfile.Load(path, &cfg); err != nil {
		return nil, err
	}
	if cfg.Provider.Name == "" {
		return nil, fmt.Errorf("config: provider.name is required")
	}
	if cfg.CreatePauseMilliseconds == nil {
		return nil, fmt.Errorf("config: create_pause_milliseconds is required: the pause between two creates of one pass, 0 for none")
	}
	size, err := cfg.Instance.size()
	if err != nil {
		return nil, fmt.Errorf("config: instance: %w", err)
	}
	cfg.size = size
	if cfg.WarmPoolSize != nil && *cfg.WarmPoolSize < 0 {
		return nil, fmt.Errorf("config: warm_pool_size must not be negative")
	}
	if err := cfg.launch().Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

const defaultWarmPool = 2

// warmPool is the pool the launcher keeps on p: the configured size, or the default where p can
// warm instances.
func (cfg *config) warmPool(p provider.Provider) int {
	if cfg.WarmPoolSize != nil {
		return *cfg.WarmPoolSize
	}
	if _, warms := p.(provider.Warmer); warms {
		return defaultWarmPool
	}
	return 0
}

// launch is the launcher's configuration as the file decides it. The token, the provider, the
// warm pool that depends on it (warmPool), the HTTP client and the log are the process's to add.
func (cfg *config) launch() agentlaunch.Config {
	return agentlaunch.Config{
		Name:        cfg.LauncherName,
		APIURL:      cfg.APIURL,
		Command:     cfg.Instance.Command,
		Size:        cfg.size,
		Interval:    time.Duration(cfg.ReconcileIntervalSeconds) * time.Second,
		CreatePause: time.Duration(*cfg.CreatePauseMilliseconds) * time.Millisecond,
	}
}

// size parses the quantities, written as a job writes its own ("2", "4Gi").
func (i instanceConfig) size() (agentlaunch.Size, error) {
	cpu, err := resource.ParseQuantity(i.CPU)
	if err != nil {
		return agentlaunch.Size{}, fmt.Errorf("cpu %q: %w", i.CPU, err)
	}
	memory, err := resource.ParseQuantity(i.Memory)
	if err != nil {
		return agentlaunch.Size{}, fmt.Errorf("memory %q: %w", i.Memory, err)
	}
	storage, err := resource.ParseQuantity(i.Storage)
	if err != nil {
		return agentlaunch.Size{}, fmt.Errorf("storage %q: %w", i.Storage, err)
	}
	return agentlaunch.Size{CPUMillis: cpu.MilliValue(), MemoryBytes: memory.Value(), DiskBytes: storage.Value()}, nil
}
