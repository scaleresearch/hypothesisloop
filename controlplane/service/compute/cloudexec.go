// Package compute runs each node of a job as one rented instance through a provider.Provider,
// the nodes of a job that spans several on a private network of the provider's. It names no
// vendor: everything it knows about an instance is the name it gave it and the
// state the provider reports.
package compute

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/acceleratorname"
	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// Shape is one kind of instance this account may rent, and how many of them at once. Each of its
// MaxInstances is reported to the control plane as a slot, a node that holds one job. Admission
// sizes a job to the slot it places it on and says which slot that is, so an instance is rented
// at its shape's size.
type Shape struct {
	// Name is part of every instance name and slot name of this shape.
	Name string
	// AcceleratorType is the resource catalog string jobs request. Empty for a shape with no
	// accelerators.
	AcceleratorType string
	Accelerators    int
	CPUMillis       int64
	MemoryBytes     int64
	StorageBytes    int64
	// MaxInstances bounds what this account can be billed for at once.
	MaxInstances int
}

type Config struct {
	// ClusterID is the provider's name, the account on it and Namespace, which name the instances
	// this cluster owns and stay the same when the cluster is renamed.
	ClusterID string
	// Namespace is part of every instance name. This executor lists, counts and destroys the
	// instances of its namespace and no others, so several agents, and whatever else is rented
	// on the account, can share it. Changed while instances exist, it orphans them.
	Namespace string
	// APIURL is handed to every job as HYPOTHESISLOOP_API_URL, so it must be reachable from a
	// rented instance.
	APIURL   string
	Shapes   []Shape
	Provider provider.Provider
	// OfferWait is how long a job may wait from its admission for a machine to rent before it is
	// reported as one this cluster cannot run.
	OfferWait time.Duration
	// Startup is how long a rented instance may take to start its command. Past it the instance
	// is destroyed and another rented, so nothing rented and not running is paid for longer.
	Startup time.Duration
	// Demand is what waits for the cluster, required for a provider with a market or a network.
	Demand Demand
	Log    func(format string, args ...any)
}

// Executor keeps nothing about instances. Every method reads the account, unless the executor
// came from ForPass, in which case it answers from that pass's one read.
type Executor struct {
	clusterID string
	prefix    string
	apiURL    string
	shapes    []Shape
	provider  provider.Provider
	// market is the provider's Market capability, nil for a vendor that has none.
	market provider.Market
	// network is the provider's Networker capability, nil for a vendor that has none.
	network       provider.Networker
	demand        Demand
	architectures []string
	// imageNeeds is what the provider's ImageNeeder says a job's image must have, nil for one
	// that is no ImageNeeder.
	imageNeeds []string
	offerWait  time.Duration
	startup    time.Duration
	log        func(format string, args ...any)
	pass       *view
}

var (
	_ agentexec.Executor   = (*Executor)(nil)
	_ agentexec.PassScoped = (*Executor)(nil)
)

// The longest experiment id and attempt number an instance name is sized for at startup. Ids are
// the submitter's to choose; one as long as a DNS label fits, and CreateWorkload refuses a job
// whose name would not.
const (
	experimentIDBudget = 63
	attemptBudget      = 9999
)

func New(cfg Config) (*Executor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Provider == nil || cfg.Log == nil {
		return nil, fmt.Errorf("cloudcompute: Provider and Log are required")
	}
	rented := provider.Bound(cfg.Provider)
	if (rented.Market != nil || rented.Networker != nil) && cfg.Demand == nil {
		return nil, fmt.Errorf("cloudcompute: a provider with a market or a network needs the cluster's Demand")
	}
	var imageNeeds []string
	if needer, needs := cfg.Provider.(provider.ImageNeeder); needs {
		imageNeeds = needer.ImageNeeds()
	}
	if rented.Networker != nil && !slices.Contains(clusterapi.NodesLevels, clusterapi.NodesLevel(rented.Networker.Within())) {
		return nil, fmt.Errorf("cloudcompute: the provider's places are within %q, none of %v", rented.Networker.Within(), clusterapi.NodesLevels)
	}
	architectures := rented.Architectures()
	if len(architectures) == 0 {
		return nil, fmt.Errorf("cloudcompute: the provider rents machines of no architecture")
	}
	return &Executor{
		market:        rented.Market,
		network:       rented.Networker,
		demand:        cfg.Demand,
		architectures: architectures,
		imageNeeds:    imageNeeds,
		clusterID:     cfg.ClusterID,
		prefix:        cfg.prefix(),
		apiURL:        cfg.APIURL,
		shapes:        append([]Shape(nil), cfg.Shapes...),
		provider:      rented,
		offerWait:     cfg.OfferWait,
		startup:       cfg.Startup,
		log:           cfg.Log,
	}, nil
}

func (cfg Config) prefix() string { return provider.JobPrefix(cfg.Namespace) }

// Validate checks everything but the provider, so a config file is refused when it is loaded,
// before any vendor is contacted.
func (cfg Config) Validate() error {
	if cfg.ClusterID == "" || cfg.APIURL == "" {
		return fmt.Errorf("cloudcompute: ClusterID and APIURL are required")
	}
	if err := provider.ValidateSegment("namespace", cfg.Namespace); err != nil {
		return fmt.Errorf("cloudcompute: %w", err)
	}
	if cfg.OfferWait <= 0 || cfg.Startup <= 0 {
		return fmt.Errorf("cloudcompute: OfferWait and Startup must be positive")
	}
	if len(cfg.Shapes) == 0 {
		return fmt.Errorf("cloudcompute: at least one shape is required")
	}
	longest := (&Executor{prefix: cfg.prefix()}).instanceName
	ranks := 0
	for _, s := range cfg.Shapes {
		ranks += s.MaxInstances
	}
	seen := map[string]bool{}
	for _, s := range cfg.Shapes {
		if err := provider.ValidateSegment("shape name", s.Name); err != nil {
			return fmt.Errorf("cloudcompute: %w", err)
		}
		if seen[s.Name] {
			return fmt.Errorf("cloudcompute: shape name %q is used twice", s.Name)
		}
		seen[s.Name] = true
		if s.CPUMillis <= 0 || s.MemoryBytes <= 0 || s.StorageBytes <= 0 || s.MaxInstances <= 0 {
			return fmt.Errorf("cloudcompute: shape %q needs positive cpu, memory, storage and max_instances", s.Name)
		}
		if (s.Accelerators > 0) != (s.AcceleratorType != "") || s.Accelerators < 0 {
			return fmt.Errorf("cloudcompute: shape %q must set accelerator_type and accelerators together", s.Name)
		}
		if s.AcceleratorType != "" {
			if err := acceleratorname.Validate(s.AcceleratorType); err != nil {
				return fmt.Errorf("cloudcompute: shape %q: %w", s.Name, err)
			}
		}
		if err := provider.ValidateName(longest(strings.Repeat("x", experimentIDBudget), attemptBudget, ranks-1, ranks, s.Name, s.MaxInstances-1)); err != nil {
			return fmt.Errorf("cloudcompute: shape %q leaves no room for an experiment id in an instance name: %w", s.Name, err)
		}
	}
	return nil
}

// ForPass reads the account once and returns an executor that answers the whole pass from that
// read. Nothing of it outlives the pass.
func (e *Executor) ForPass(ctx context.Context) (agentexec.Executor, error) {
	v, err := e.read(ctx)
	if err != nil {
		return nil, err
	}
	scoped := *e
	scoped.pass = v
	return &scoped, nil
}

func (e *Executor) GetClusterID(context.Context) (string, error) { return e.clusterID, nil }

// SupportsMultiNodeJobs is whether the provider has private networks to join the nodes of a job
// on.
func (e *Executor) SupportsMultiNodeJobs() bool { return e.network != nil }
