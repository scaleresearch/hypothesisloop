// hypothesisloop-cloud-agent runs jobs on rented instances: one process per provider account,
// registered as one cluster. The reconcile/status loop lives in runtime/shared/agentloop; this
// file builds the Executor from the one config file CLOUD_AGENT_CONFIG names.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

// Every pass reads the provider's API several times, so the loops run slower than a runtime
// reading a local engine, and still several times inside the control plane's unreachable window.
const (
	binaryName        = "cloud-agent"
	reconcileInterval = 5 * time.Second
	statusInterval    = 5 * time.Second
)

func main() {
	cfg, err := loadConfig(agentloop.RequiredEnv(binaryName, "CLOUD_AGENT_CONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}
	token := agentloop.RequiredEnv(binaryName, "RUNTIME_TOKEN")
	rented, err := newProvider(cfg.Provider)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}
	exec, err := cloudexec.New(cloudexec.Config{
		ClusterID: cfg.Provider.Name + "-" + cfg.Provider.AccountID,
		APIURL:    cfg.APIURL,
		JobImage:  cfg.JobImage,
		Shapes:    cfg.shapes,
		Provider:  rented,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud-agent: %v\n", err)
		os.Exit(1)
	}

	log := func(format string, args ...any) {
		fmt.Printf("[cloud-agent] "+format+"\n", args...)
	}
	ctx, cancel := agentloop.SignalContext(log)
	defer cancel()

	log("starting: cluster=%s provider=%s account=%s control_plane=%s shapes=%d", cfg.ClusterName, cfg.Provider.Name, cfg.Provider.AccountID, cfg.APIURL, len(cfg.shapes))

	a := &agentloop.Agent{
		ClusterName:       cfg.ClusterName,
		APIURL:            cfg.APIURL,
		Token:             token,
		Executor:          exec,
		HTTPClient:        &http.Client{Timeout: 35 * time.Second},
		ReconcileInterval: reconcileInterval,
		StatusInterval:    statusInterval,
		Log:               log,
	}
	a.Run(ctx)
}
