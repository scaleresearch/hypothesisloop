package main

import (
	"cmp"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/providers"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const binaryName = "agent-launcher"

// version is the image tag this binary was built from, set at link time (-X main.version=TAG).
// Built without it, the launcher names no version.
var version string

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
	log.SetPrefix("[agent-launcher] ")
	log.SetFlags(log.LstdFlags | log.LUTC | log.Lmsgprefix)
	launch := cfg.launch()
	launch.Token = agentloop.RequiredEnv(binaryName, "LAUNCHER_TOKEN")
	launch.Version = version
	launch.Provider = rented
	launch.WarmPool = cfg.warmPool(rented)
	launch.HTTPClient = &http.Client{Timeout: 35 * time.Second}
	launch.Log = log.Printf
	launcher, err := agentlaunch.New(launch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-launcher: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := agentloop.SignalContext(log.Printf)
	defer cancel()
	log.Printf("starting: version=%s launcher=%s provider=%s capabilities=%v control_plane=%s interval=%s create_pause=%s warm_pool=%d",
		cmp.Or(version, "unknown"), cfg.LauncherName, cfg.Provider.Name, provider.Capabilities(rented), cfg.APIURL, launch.Interval, launch.CreatePause, launch.WarmPool)
	if err := launcher.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agent-launcher: %v\n", err)
		os.Exit(1)
	}
}
