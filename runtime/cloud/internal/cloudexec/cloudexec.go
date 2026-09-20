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
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// Shape is one kind of instance this account may rent, and how many of them at once. Each of its
// MaxInstances slots is reported to the control plane as a node, so admission places a job on
// rented compute the way it places one on any cluster. An instance is sized by the job it runs;
// the shape is the most one slot may be asked for.
type Shape struct {
	// Name is part of every instance name and node name of this shape.
	Name string
	// AcceleratorType is the resource catalog string jobs request, written exactly as they write
	// it: the control plane compares the type a running job reports with the one it asked for.
	// Empty for a shape with no accelerators.
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
	// APIURL is handed to every job as HYPOTHESISLOOP_API_URL, so it must be reachable from a
	// rented instance.
	APIURL string
	// JobImage is the image every instance boots. It carries hl, which the job's command runs
	// under, so a job's own image field is not used on this runtime.
	JobImage string
	Shapes   []Shape
	Provider provider.Provider
}

type Executor struct {
	clusterID string
	apiURL    string
	jobImage  string
	shapes    []Shape
	provider  provider.Provider
}

var _ agentexec.Executor = (*Executor)(nil)

var shapeNamePattern = regexp.MustCompile(`^[a-z0-9]+$`)

func New(cfg Config) (*Executor, error) {
	if cfg.ClusterID == "" || cfg.APIURL == "" || cfg.JobImage == "" || cfg.Provider == nil {
		return nil, fmt.Errorf("cloudexec: ClusterID, APIURL, JobImage and Provider are required")
	}
	if len(cfg.Shapes) == 0 {
		return nil, fmt.Errorf("cloudexec: at least one shape is required")
	}
	seen := map[string]bool{}
	for _, s := range cfg.Shapes {
		// The name is parsed back out of an instance name from the right, so it holds no dash.
		if !shapeNamePattern.MatchString(s.Name) {
			return nil, fmt.Errorf("cloudexec: shape name %q must be lowercase letters and digits", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("cloudexec: shape name %q is used twice", s.Name)
		}
		seen[s.Name] = true
		if s.CPUMillis <= 0 || s.MemoryBytes <= 0 || s.StorageBytes <= 0 || s.MaxInstances <= 0 {
			return nil, fmt.Errorf("cloudexec: shape %q needs positive cpu, memory, storage and max_instances", s.Name)
		}
		if (s.Accelerators > 0) != (s.AcceleratorType != "") || s.Accelerators < 0 {
			return nil, fmt.Errorf("cloudexec: shape %q must set accelerator_type and accelerators together", s.Name)
		}
		if _, _, ok := strings.Cut(s.AcceleratorType, "="); s.AcceleratorType != "" && !ok {
			return nil, fmt.Errorf("cloudexec: shape %q accelerator_type %q is not a key=value catalog string", s.Name, s.AcceleratorType)
		}
	}
	return &Executor{
		clusterID: cfg.ClusterID,
		apiURL:    cfg.APIURL,
		jobImage:  cfg.JobImage,
		shapes:    append([]Shape(nil), cfg.Shapes...),
		provider:  bounded{cfg.Provider},
	}, nil
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
