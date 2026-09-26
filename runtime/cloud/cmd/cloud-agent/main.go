// hypothesisloop-cloud-agent runs jobs on rented instances: one process per provider account,
// registered as one cluster. The reconcile/status loop lives in runtime/shared/agentloop; this
// file builds the Executor from the one config file CLOUD_AGENT_CONFIG names.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

const binaryName = "cloud-agent"

func main() {
	cfg, err := loadConfig(agentloop.RequiredEnv(binaryName, "CLOUD_AGENT_CONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}
	token := agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN")
	rented, err := newProvider(cfg.Provider, cfg.shapes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}
	exec, err := cloudexec.New(cfg.executor(rented))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[cloud-agent] "+format+"\n", args...)
	}
	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()

	log("starting: cluster=%s provider=%s capabilities=%v account=%s namespace=%s control_plane=%s shapes=%d", cfg.ClusterName, cfg.Provider.Name, provider.Capabilities(rented), cfg.Provider.AccountID, cfg.Namespace, cfg.APIURL, len(cfg.shapes))

	a := &agentloop.Agent{
		ClusterName:       cfg.ClusterName,
		APIURL:            cfg.APIURL,
		Token:             token,
		Executor:          exec,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: time.Duration(cfg.ReconcileIntervalSeconds) * time.Second,
		StatusInterval:    time.Duration(cfg.StatusIntervalSeconds) * time.Second,
		LogTailLines:      cfg.LogTailLines,
		Log:               log,
	}
	a.Run(ctx)

	leaving, cancelLeaving := context.WithTimeout(context.Background(), leaveLimit)
	defer cancelLeaving()
	if err := exec.Leave(leaving); err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}
}

// leaveLimit bounds what the process still does for its rentals after it was told to end, under
// the grace a supervisor gives a process before it kills it.
const leaveLimit = 20 * time.Second
