package cloudcompute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
)

// Exchange is the control plane's side of a cluster's reconcile and status push
// (clusteragentapi.Handler).
type Exchange interface {
	Reconcile(ctx context.Context, clusterName, agentVersion string, report clusterapi.ReconcileRequest) (clusterapi.ReconcileAnswer, error)
	PushStatus(ctx context.Context, clusterName string, push clusterapi.StatusPush) error
}

// Registry says whether a cluster is registered (db.ClustersStore).
type Registry interface {
	ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error)
}

// controlPlane is agentloop.ControlPlane in process: the cluster's calls go to the exchange
// directly, and a cluster nobody registered is agentloop.ErrClusterRemoved, as a runtime's token
// of one is refused.
type controlPlane struct {
	exchange Exchange
	registry Registry
	cluster  string
	version  string
}

func (c controlPlane) registered(ctx context.Context) error {
	_, found, err := c.registry.ClusterOrg(ctx, c.cluster)
	if err != nil {
		return err
	}
	if !found {
		return agentloop.ErrClusterRemoved
	}
	return nil
}

func (c controlPlane) Reconcile(ctx context.Context, report clusterapi.ReconcileRequest) (clusterapi.ReconcileAnswer, error) {
	if err := c.registered(ctx); err != nil {
		return clusterapi.ReconcileAnswer{}, err
	}
	return c.exchange.Reconcile(ctx, c.cluster, c.version, report)
}

func (c controlPlane) PushStatus(ctx context.Context, push clusterapi.StatusPush) error {
	if err := c.registered(ctx); err != nil {
		return err
	}
	return c.exchange.PushStatus(ctx, c.cluster, push)
}

// ClusterConfig is one provider account run as the registered cluster Name.
type ClusterConfig struct {
	Name     string
	Executor Config
	Exchange Exchange
	Registry Registry
	// Version is what the cluster's runtime is said to run: the control plane's own.
	Version           string
	ReconcileInterval time.Duration
	StatusInterval    time.Duration
	Log               func(format string, args ...any)
}

// Cluster is the runtime of one cloud compute account, run inside the control plane.
type Cluster struct {
	cfg  ClusterConfig
	exec *Executor
}

func NewCluster(cfg ClusterConfig) (*Cluster, error) {
	if cfg.Name == "" || cfg.Exchange == nil || cfg.Registry == nil || cfg.Log == nil || cfg.ReconcileInterval <= 0 || cfg.StatusInterval <= 0 {
		return nil, errors.New("cloudcompute: Name, Exchange, Registry, Log and both intervals are required")
	}
	exec, err := New(cfg.Executor)
	if err != nil {
		return nil, fmt.Errorf("cloudcompute: cluster %s: %w", cfg.Name, err)
	}
	return &Cluster{cfg: cfg, exec: exec}, nil
}

// leaveLimit bounds what is still done for the rentals once ctx ended.
const leaveLimit = 20 * time.Second

// Run keeps the cluster's rentals in step with its desired state until ctx ends. While the
// cluster is not registered it desires nothing: what it holds is stopped, and nothing is rented
// until it is registered again.
func (c *Cluster) Run(ctx context.Context) {
	removed := false
	for ctx.Err() == nil {
		agent := &agentloop.Agent{
			ClusterName:       c.cfg.Name,
			ControlPlane:      controlPlane{exchange: c.cfg.Exchange, registry: c.cfg.Registry, cluster: c.cfg.Name, version: c.cfg.Version},
			Executor:          c.exec,
			ReconcileInterval: c.cfg.ReconcileInterval,
			StatusInterval:    c.cfg.StatusInterval,
			Log:               c.cfg.Log,
		}
		err := agent.Run(ctx)
		if !errors.Is(err, agentloop.ErrClusterRemoved) {
			break
		}
		if !removed {
			c.cfg.Log("cluster %s is not registered: nothing is rented for it until it is (hl-admin cluster add %s)", c.cfg.Name, c.cfg.Name)
		}
		removed = true
		select {
		case <-ctx.Done():
		case <-time.After(c.cfg.ReconcileInterval):
		}
	}
	leaving, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaveLimit)
	defer cancel()
	if err := c.exec.Leave(leaving); err != nil {
		c.cfg.Log("%v", err)
	}
}
