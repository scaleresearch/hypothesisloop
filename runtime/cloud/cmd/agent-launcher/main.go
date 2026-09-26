package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/providers"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const binaryName = "agent-launcher"

func main() {
	cfg, err := loadConfig(agentloop.RequiredEnv(binaryName, "AGENT_LAUNCHER_CONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-launcher: %v\n", err)
		os.Exit(1)
	}
	// An agent instance has no accelerator, so the provider is asked for no accelerator type.
	rented, err := providers.New(cfg.Provider.Name, cfg.Provider.Settings, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-launcher: config: %v\n", err)
		os.Exit(1)
	}
	log := func(format string, args ...any) {
		fmt.Printf("[agent-launcher] "+format+"\n", args...)
	}
	launch := cfg.launch()
	launch.Token = agentloop.RequiredEnv(binaryName, "LAUNCHER_TOKEN")
	launch.Provider = rented
	launch.HTTPClient = &http.Client{Timeout: 35 * time.Second}
	launch.Log = log
	launcher, err := agentlaunch.New(launch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-launcher: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()
	log("starting: launcher=%s provider=%s capabilities=%v control_plane=%s model_provider=%s interval=%s create_pause=%s",
		cfg.LauncherName, cfg.Provider.Name, provider.Capabilities(rented), cfg.APIURL, cfg.ModelProvider.Name, launch.Interval, launch.CreatePause)
	if err := launcher.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agent-launcher: %v\n", err)
		os.Exit(1)
	}
}
