// Package clusterexchange serves what cluster-agents call; the control plane never calls out.
// There is no command/ack protocol: an experiment's status (SUBMITTED/RUNNING) alone says whether
// a Job should exist, and each agent reconciles its Jobs to that view, so any number of agent
// replicas may poll it without coordination.
package clusterexchange

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
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
	// RecordContactRefusal keeps what a reconcile of the cluster was refused with, and clears it
	// with an empty refusal (db.Store).
	RecordContactRefusal(ctx context.Context, name, refusal string) error
	// RemoveCluster ends a cluster and returns its jobs to the queue (db.Store).
	RemoveCluster(ctx context.Context, name string) (requeued []string, err error)
}

// ImagePulls mints the login a job's runtime pulls its image with (registrycred.PullFor), nil
// for an image pulled without one.
type ImagePulls func(ctx context.Context, orgID, image string) (*domain.RegistryLogin, error)

// Service is the control plane's side of the exchange with cluster-agents.
type Service struct {
	store  Store
	logger *zap.Logger
	// connectedWithin is how recent a heartbeat must be to count as "connected".
	connectedWithin time.Duration
	// metrics is where live per-cluster accelerator capacity is written.
	metrics *metricsdb.Store
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
	// pulls mints each desired job's image pull login.
	pulls ImagePulls
}

// NewService constructs a Service. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewService(store Store, connectedWithin time.Duration, metrics *metricsdb.Store, observationCadence time.Duration, dataStore *objectstore.Client, maxCheckpointGrace time.Duration, pricedAcceleratorTypes []string, maxLogLineChars int, signingSecret, proxyURL string, pulls ImagePulls, logger *zap.Logger) *Service {
	priced := make(map[string]bool, len(pricedAcceleratorTypes))
	for _, name := range pricedAcceleratorTypes {
		priced[name] = true
	}
	return &Service{store: store, connectedWithin: connectedWithin, metrics: metrics, observationCadence: observationCadence,
		dataStore: dataStore, maxCheckpointGrace: maxCheckpointGrace,
		pricedAcceleratorTypes: priced, maxLogLineChars: maxLogLineChars, signingSecret: signingSecret, proxyURL: proxyURL, pulls: pulls, logger: logger}
}

// keepPricedAccelerators drops every accelerator type outside the priced catalog from report.
func (s *Service) keepPricedAccelerators(report *clusterapi.ReconcileRequest) {
	unpriced := func(acceleratorType string) bool { return !s.pricedAcceleratorTypes[acceleratorType] }
	for _, byType := range []map[string]int64{report.AcceleratorAvailableByType, report.AcceleratorTotalByType} {
		for acceleratorType := range byType {
			if unpriced(acceleratorType) {
				delete(byType, acceleratorType)
			}
		}
	}
	for _, byType := range report.AcceleratorAvailableByNode {
		for acceleratorType := range byType {
			if unpriced(acceleratorType) {
				delete(byType, acceleratorType)
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
