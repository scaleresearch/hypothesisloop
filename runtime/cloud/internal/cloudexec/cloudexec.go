// Package cloudexec runs each job as one rented instance through a provider.Provider. It names
// no vendor: everything it knows about an instance is the name it gave it and the state the
// provider reports.
package cloudexec

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/acceleratorname"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// Shape is one kind of instance this account may rent, and how many of them at once. Each of its
// MaxInstances is reported to the control plane as a slot, a node that holds one job, so
// admission places a job on rented compute the way it places one on any cluster and says which
// slot it placed it on. An instance is sized by the job it runs; the shape is the most one slot
// may be asked for.
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
	// ClusterID is the provider's name and the account on it, which is what stays the same when
	// the cluster is renamed.
	ClusterID string
	// Namespace is part of every instance name. This executor lists, counts and destroys the
	// instances of its namespace and no others, so several agents, and whatever else is rented
	// on the account, can share it.
	Namespace string
	// APIURL is handed to every job as HYPOTHESISLOOP_API_URL, so it must be reachable from a
	// rented instance.
	APIURL string
	// JobImage is the image every instance boots. It carries hl, which the job's command runs
	// under, so a job that names another image is refused.
	JobImage string
	// LogTailLines is how much of an instance's output one pass reads.
	LogTailLines int
	Shapes       []Shape
	Provider     provider.Provider
}

// Executor keeps nothing about instances. Every method reads the account, unless the executor
// came from ForPass, in which case it answers from that pass's one read.
type Executor struct {
	clusterID    string
	prefix       string
	apiURL       string
	jobImage     string
	logTailLines int
	shapes       []Shape
	provider     provider.Provider
	pass         *view
}

var (
	_ agentexec.Executor   = (*Executor)(nil)
	_ agentexec.PassScoped = (*Executor)(nil)
)

// segmentPattern is what a namespace and a shape name may be. Neither holds a dash: an instance
// name is read back from the right, and a namespace with one could be the prefix of another's.
var segmentPattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

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
	if cfg.Provider == nil {
		return nil, fmt.Errorf("cloudexec: Provider is required")
	}
	return &Executor{
		clusterID:    cfg.ClusterID,
		prefix:       cfg.prefix(),
		apiURL:       cfg.APIURL,
		jobImage:     cfg.JobImage,
		logTailLines: cfg.LogTailLines,
		shapes:       append([]Shape(nil), cfg.Shapes...),
		provider:     bounded{cfg.Provider},
	}, nil
}

func (cfg Config) prefix() string { return jobPrefix + cfg.Namespace + "-" }

// Validate checks everything but the provider, so a config file is refused when it is loaded,
// before any vendor is contacted.
func (cfg Config) Validate() error {
	if cfg.ClusterID == "" || cfg.APIURL == "" || cfg.JobImage == "" {
		return fmt.Errorf("cloudexec: ClusterID, APIURL and JobImage are required")
	}
	if !segmentPattern.MatchString(cfg.Namespace) {
		return fmt.Errorf("cloudexec: namespace %q must be 1 to 16 lowercase letters and digits", cfg.Namespace)
	}
	if cfg.LogTailLines <= 0 {
		return fmt.Errorf("cloudexec: LogTailLines must be positive")
	}
	if len(cfg.Shapes) == 0 {
		return fmt.Errorf("cloudexec: at least one shape is required")
	}
	longest := (&Executor{prefix: cfg.prefix()}).instanceName
	seen := map[string]bool{}
	for _, s := range cfg.Shapes {
		if !segmentPattern.MatchString(s.Name) {
			return fmt.Errorf("cloudexec: shape name %q must be 1 to 16 lowercase letters and digits", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("cloudexec: shape name %q is used twice", s.Name)
		}
		seen[s.Name] = true
		if s.CPUMillis <= 0 || s.MemoryBytes <= 0 || s.StorageBytes <= 0 || s.MaxInstances <= 0 {
			return fmt.Errorf("cloudexec: shape %q needs positive cpu, memory, storage and max_instances", s.Name)
		}
		if (s.Accelerators > 0) != (s.AcceleratorType != "") || s.Accelerators < 0 {
			return fmt.Errorf("cloudexec: shape %q must set accelerator_type and accelerators together", s.Name)
		}
		if s.AcceleratorType != "" {
			if err := acceleratorname.Validate(s.AcceleratorType); err != nil {
				return fmt.Errorf("cloudexec: shape %q: %w", s.Name, err)
			}
		}
		if err := provider.ValidateName(longest(strings.Repeat("x", experimentIDBudget), attemptBudget, s.Name, s.MaxInstances-1)); err != nil {
			return fmt.Errorf("cloudexec: shape %q leaves no room for an experiment id in an instance name: %w", s.Name, err)
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

// SupportsMultiNodeJobs is false: a job is one instance, and instances of one account share no
// network a distributed job could rendezvous over.
func (e *Executor) SupportsMultiNodeJobs() bool { return false }

// providerCallTimeout bounds every call to the vendor, so an API that stops answering fails the
// pass instead of stalling the reconcile and status loops.
const providerCallTimeout = time.Minute

type bounded struct{ provider.Provider }

func (b bounded) Create(ctx context.Context, spec provider.Spec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	return b.Provider.Create(ctx, spec)
}

func (b bounded) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	return b.Provider.List(ctx, namePrefix)
}

func (b bounded) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	return b.Provider.Logs(ctx, id, tail)
}

func (b bounded) Stop(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	return b.Provider.Stop(ctx, id)
}

func (b bounded) Destroy(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()
	return b.Provider.Destroy(ctx, id)
}
