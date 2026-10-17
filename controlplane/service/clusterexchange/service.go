// Package clusterexchange serves what cluster-agents call; the control plane never calls out.
// There is no command/ack protocol: an experiment's status (SUBMITTED/RUNNING) alone says whether
// a Job should exist, and each agent reconciles its Jobs to that view, so any number of agent
// replicas may poll it without coordination.
package clusterexchange

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// Store is the desired-state persistence interface the handler needs.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error)
	// ListRecentlyUndesiredWorkloads returns the experiments last placed on the cluster that have
	// just stopped being desired, the ones its agent is about to delete, so the reconcile
	// response can name which of them earned a checkpoint window.
	ListRecentlyUndesiredWorkloads(ctx context.Context, clusterName string, within time.Duration) ([]*domain.Experiment, error)
	// PlacementByID resolves many experiments' assigned cluster and current attempt at once — a
	// status push is a whole-cluster snapshot, so this is asked once per push rather than once
	// per job.
	PlacementByID(ctx context.Context, ids []string) (map[string]domain.Placement, error)
	// RunFirstReportedAt is when the end of each job's run was first reported under its current
	// admission, which this call is for a job no report had named. A job that holds no capacity
	// has none.
	RunFirstReportedAt(ctx context.Context, ids []string) (map[string]time.Time, error)
	// BindReportedID records, once, the identity a registered cluster's runtime states about
	// itself. It refuses one another cluster holds (db.ErrClusterIDBound) and one other than
	// the cluster's own (db.ErrClusterIDChanged).
	BindReportedID(ctx context.Context, name, reportedID string) error
	// RecordJobImage keeps the one image the cluster's runtime says it runs.
	RecordJobImage(ctx context.Context, name, image string) error
	// RecordContactRefusal keeps what a reconcile of the cluster was refused with, and clears it
	// with an empty refusal (db.Store).
	RecordContactRefusal(ctx context.Context, name, refusal string) error
	// RemoveCluster ends a cluster and returns its jobs to the queue (db.Store).
	RemoveCluster(ctx context.Context, name string) (requeued []string, err error)
	// RecordBuilds keeps whether the cluster's runtime builds environments.
	RecordBuilds(ctx context.Context, name string, builds bool) error
	// AssignedEnvironmentBuilds, AssignedEnvironmentBuild and FinishEnvironmentBuild are the
	// builds the scheduler assigned to a cluster (db.Store).
	AssignedEnvironmentBuilds(ctx context.Context, cluster string) ([]*domain.Environment, error)
	AssignedEnvironmentBuild(ctx context.Context, cluster, key string, attempt int) (*domain.Environment, error)
	FinishEnvironmentBuild(ctx context.Context, cluster, key string, attempt int, outcome domain.EnvironmentBuildOutcome) (bool, error)
	RecordEnvironmentProgress(ctx context.Context, cluster, key string, attempt int, step string) (bool, error)
	RecordEnvironmentLoaded(ctx context.Context, cluster, key string, attempt int, node, image string) (bool, error)
}

// RegistryLogins hands out logins to the registry of environment images (registrycred.Source).
type RegistryLogins interface {
	Push(ctx context.Context) (domain.RegistryLogin, error)
	Pull(ctx context.Context) (domain.RegistryLogin, error)
}

// RepositoryCredentials mints what git reads a repository of a project with (githost.Provider).
type RepositoryCredentials interface {
	URL(repository string) string
	Credential(ctx context.Context, projectID, repository string, write bool) (*githost.Credential, error)
}

// EnvironmentRegistry is where assigned builds push to, and the logins to it. Git reads the
// repository an environment file is of, for its build.
type EnvironmentRegistry struct {
	Repository string
	Logins     RegistryLogins
	Git        RepositoryCredentials
}

// Service is the control plane's side of the exchange with cluster-agents.
type Service struct {
	store  Store
	logger *zap.Logger
	// connectedWithin is how recent a heartbeat must be to count as "connected".
	connectedWithin time.Duration
	// metricsDBURL is where live per-cluster accelerator capacity is written.
	metricsDBURL string
	// observationCadence is the longest gap between two observations that still counts as one
	// run (config.SchedulerConfig.ObservationCadence).
	observationCadence time.Duration
	// dataStore supplies the durable-data addressing attached to every desired workload on its
	// way out. Only the address is read from it; the reconcile pass never calls the store.
	dataStore *objectstore.Client
	// maxCheckpointGrace is how far back a reconcile pass looks for workloads that have just
	// left the desired set. It is the configured ceiling on a checkpoint window, which is the
	// longest a granted window can possibly still be running: past it, the grant has expired on
	// its own and there is nothing to hold or clear.
	maxCheckpointGrace time.Duration
	// pricedAcceleratorTypes holds the catalog's accelerator types, lowercased (driver casing and
	// catalog casing can differ, see domain.AcceleratorType.MatchesLabels). Hardware publishes
	// many true facts about itself (arch, board, tray, serial); capacity keeps only the types
	// that can be billed, since admission refuses anything unpriced.
	pricedAcceleratorTypes map[string]bool
	// maxLogLineChars splits longer reported log lines, never truncating them.
	maxLogLineChars int
	// signingSecret derives each job's own token, handed out beside its data key.
	signingSecret string
	// proxyURL is where the control plane's git proxy serves repositories, which each job fetches
	// its commit from with its token.
	proxyURL string
	// environments is nil on a deployment that builds no environments, which counts no cluster
	// as building.
	environments *EnvironmentRegistry
	// buildReported wakes admission once a build is loaded on a node or ends, so the jobs that
	// wait for it are placed without waiting for the next heartbeat.
	buildReported func()
}

// WithBuildReported has every recorded build report that can place a job call wake.
func (s *Service) WithBuildReported(wake func()) *Service {
	s.buildReported = wake
	return s
}

func (s *Service) wakeAdmission() {
	if s.buildReported != nil {
		s.buildReported()
	}
}

// NewService constructs a Service. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewService(store Store, connectedWithin time.Duration, metricsDBURL string, observationCadence time.Duration, dataStore *objectstore.Client, maxCheckpointGrace time.Duration, pricedAcceleratorTypes []string, maxLogLineChars int, signingSecret, proxyURL string, environments *EnvironmentRegistry, logger *zap.Logger) *Service {
	priced := make(map[string]bool, len(pricedAcceleratorTypes))
	for _, name := range pricedAcceleratorTypes {
		priced[strings.ToLower(name)] = true
	}
	return &Service{store: store, connectedWithin: connectedWithin, metricsDBURL: metricsDBURL, observationCadence: observationCadence,
		dataStore: dataStore, maxCheckpointGrace: maxCheckpointGrace,
		pricedAcceleratorTypes: priced, maxLogLineChars: maxLogLineChars, signingSecret: signingSecret, proxyURL: proxyURL, environments: environments, logger: logger}
}

// keepPricedAccelerators drops every accelerator type outside the priced catalog from report.
func (s *Service) keepPricedAccelerators(report *clusterapi.ReconcileRequest) {
	unpriced := func(flavor string) bool { return !s.pricedAcceleratorTypes[strings.ToLower(flavor)] }
	for _, byFlavor := range []map[string]int64{report.AcceleratorAvailableByFlavor, report.AcceleratorTotalByFlavor} {
		for flavor := range byFlavor {
			if unpriced(flavor) {
				delete(byFlavor, flavor)
			}
		}
	}
	for _, byFlavor := range report.AcceleratorAvailableByNode {
		for flavor := range byFlavor {
			if unpriced(flavor) {
				delete(byFlavor, flavor)
			}
		}
	}
}

// splitLongLines breaks any line over maxLineChars into several, so nothing is dropped, only
// wrapped.
func splitLongLines(lines []string, maxLineChars int) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		for len(line) > maxLineChars {
			out = append(out, line[:maxLineChars])
			line = line[maxLineChars:]
		}
		out = append(out, line)
	}
	return out
}
