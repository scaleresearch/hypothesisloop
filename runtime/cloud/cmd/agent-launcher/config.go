package main

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/configfile"
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
	CreatePauseMilliseconds *int                `yaml:"create_pause_milliseconds"`
	Provider                providerConfig      `yaml:"provider"`
	Instance                instanceConfig      `yaml:"instance"`
	ModelProvider           modelProviderConfig `yaml:"model_provider"`

	size             agentlaunch.Size
	modelProviderEnv map[string]string
}

// modelProviderConfig is whose models the agents run on and what that provider's harness needs
// in every instance. The launcher adds it itself, so a credential never passes through the
// control plane or its database.
type modelProviderConfig struct {
	Name string             `yaml:"name"`
	Env  []instanceVariable `yaml:"env"`
}

// instanceVariable is one variable of every instance and the variable of this process that
// holds its value. The file names variables and never holds a value.
type instanceVariable struct {
	Name     string `yaml:"name"`
	ValueEnv string `yaml:"value_env"`
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
	// LifetimeSeconds is the longest an instance runs before it ends by itself, launcher or no
	// launcher. Left out, an instance ends only when this launcher ends it.
	LifetimeSeconds int `yaml:"lifetime_seconds"`
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
	if cfg.ModelProvider.Name == "" || len(cfg.ModelProvider.Env) == 0 {
		return nil, fmt.Errorf("config: model_provider needs a name and env: the variables every agent instance is given, each with the variable of this process that holds its value (value_env)")
	}
	cfg.modelProviderEnv = make(map[string]string, len(cfg.ModelProvider.Env))
	for _, variable := range cfg.ModelProvider.Env {
		if !variableName.MatchString(variable.Name) || !variableName.MatchString(variable.ValueEnv) {
			return nil, fmt.Errorf("config: model_provider.env entry %q: name and value_env are both names of environment variables, and a value is never written here", variable.Name)
		}
		if _, twice := cfg.modelProviderEnv[variable.Name]; twice {
			return nil, fmt.Errorf("config: model_provider.env names %s twice", variable.Name)
		}
		value := os.Getenv(variable.ValueEnv)
		if value == "" {
			return nil, fmt.Errorf("config: environment variable %s, which model_provider.env gives to instances as %s, is not set", variable.ValueEnv, variable.Name)
		}
		cfg.modelProviderEnv[variable.Name] = value
	}
	if err := cfg.launch().Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

var variableName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// launch is the launcher's configuration as the file decides it. The token, the provider, the
// HTTP client and the log are the process's to add.
func (cfg *config) launch() agentlaunch.Config {
	return agentlaunch.Config{
		Name:          cfg.LauncherName,
		APIURL:        cfg.APIURL,
		Command:       cfg.Instance.Command,
		Size:          cfg.size,
		Lifetime:      time.Duration(cfg.Instance.LifetimeSeconds) * time.Second,
		ModelProvider: cfg.ModelProvider.Name,
		Env:           cfg.modelProviderEnv,
		Interval:      time.Duration(cfg.ReconcileIntervalSeconds) * time.Second,
		CreatePause:   time.Duration(*cfg.CreatePauseMilliseconds) * time.Millisecond,
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
