package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/configfile"
)

// claudeTokenVariable is the variable an agent's harness reads its Claude credential from.
const claudeTokenVariable = "CLAUDE_CODE_OAUTH_TOKEN"

type config struct {
	// LauncherName is the name the control plane gives hosted agents to (its agents.launcher),
	// and the name this process asks for its list under.
	LauncherName string `yaml:"launcher_name"`
	// APIURL is where this process reaches the control plane. The address agents reach it at is
	// the control plane's own setting and arrives in each instance's environment.
	APIURL string `yaml:"api_url"`
	// Namespace is part of every instance name, so launchers sharing an account keep apart.
	Namespace string `yaml:"namespace"`
	// Each pass costs the provider one list call and the control plane one request.
	ReconcileIntervalSeconds int            `yaml:"reconcile_interval_seconds"`
	Provider                 providerConfig `yaml:"provider"`
	Instance                 instanceConfig `yaml:"instance"`
	// ClaudeCodeOAuthTokenEnv names the environment variable of this process that holds the
	// Claude credential every agent runs with. The launcher adds it to each instance itself, so
	// it never passes through the control plane or its database.
	ClaudeCodeOAuthTokenEnv string `yaml:"claude_code_oauth_token_env"`

	size        agentlaunch.Size
	claudeToken string
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
	size, err := cfg.Instance.size()
	if err != nil {
		return nil, fmt.Errorf("config: instance: %w", err)
	}
	cfg.size = size
	if cfg.ClaudeCodeOAuthTokenEnv == "" {
		return nil, fmt.Errorf("config: claude_code_oauth_token_env is required: the environment variable that holds the agents' Claude credential")
	}
	if cfg.claudeToken = os.Getenv(cfg.ClaudeCodeOAuthTokenEnv); cfg.claudeToken == "" {
		return nil, fmt.Errorf("config: environment variable %s, named by claude_code_oauth_token_env, is not set", cfg.ClaudeCodeOAuthTokenEnv)
	}
	if err := cfg.launch().Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// launch is the launcher's configuration as the file decides it. The token, the provider, the
// HTTP client and the log are the process's to add.
func (cfg *config) launch() agentlaunch.Config {
	return agentlaunch.Config{
		Name:      cfg.LauncherName,
		APIURL:    cfg.APIURL,
		Namespace: cfg.Namespace,
		Command:   cfg.Instance.Command,
		Size:      cfg.size,
		Env:       map[string]string{claudeTokenVariable: cfg.claudeToken},
		Interval:  time.Duration(cfg.ReconcileIntervalSeconds) * time.Second,
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
