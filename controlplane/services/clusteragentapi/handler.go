// Package clusteragentapi exposes the HTTP surface cluster-agents call — and only
// cluster-agents call. Every request in this package is initiated by an agent running
// inside a target cluster; the control plane never calls out. Endpoints:
//
//	POST /internal/clusters/{name}/reconcile — actual capacity in, desired workloads out
//	POST /internal/clusters/{name}/status        — push job phase observations
//	POST /internal/clusters/{name}/builds/{key}/credential — the push login of an assigned build
//	POST /internal/clusters/{name}/builds/{key}  — the outcome of an assigned build
//	POST /internal/clusters/{name}/builds/{key}/progress — the step an assigned build is at
//	POST /internal/clusters/{name}/builds/{key}/loaded — an assigned build held on one node
//
// There is no command/ack protocol: an experiment's own status (SUBMITTED/RUNNING)
// is the single source of truth for "should a Job exist" — the agent fetches that view and
// reconciles its local Jobs to match, the same way a kubelet reconciles pods to a desired
// spec. This means desired-state is naturally idempotent and safe to poll from any number of
// concurrent cluster-agent replicas without coordination.
//
// This is a distinct *consumer* surface from the research-agent/dashboard API: it is served
// with its own Huma registration, its own /internal/clusters/openapi.json and its own
// /internal/clusters/explore digest, tagged "cluster-agent", so the Go cluster-agent binary's
// contract is never mixed into the Python research agent's discovery doc.
package clusteragentapi

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/opencontainers/go-digest"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// Store is the desired-state persistence interface the handler needs.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string, windDown time.Duration) ([]*domain.Experiment, error)
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
	// RemoveCluster ends a cluster and returns its jobs to the queue (db.ClustersStore).
	RemoveCluster(ctx context.Context, name string) (requeued []string, err error)
	// RecordBuilds keeps whether the cluster's runtime builds environments.
	RecordBuilds(ctx context.Context, name string, builds bool) error
	// AssignedEnvironmentBuilds, AssignedEnvironmentBuild and FinishEnvironmentBuild are the
	// builds the scheduler assigned to a cluster (db.EnvironmentsStore).
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

// EnvironmentRegistry is where assigned builds push to, and the logins to it.
type EnvironmentRegistry struct {
	Repository string
	Logins     RegistryLogins
}

// Handler serves the cluster-agent-facing API.
type Handler struct {
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
	// environments is nil on a deployment that builds no environments, which counts no cluster
	// as building.
	environments *EnvironmentRegistry
	// buildReported wakes admission once a build is loaded on a node or ends, so the jobs that
	// wait for it are placed without waiting for the next heartbeat.
	buildReported func()
}

// WithBuildReported has every recorded build report that can place a job call wake.
func (h *Handler) WithBuildReported(wake func()) *Handler {
	h.buildReported = wake
	return h
}

func (h *Handler) wakeAdmission() {
	if h.buildReported != nil {
		h.buildReported()
	}
}

// NewHandler constructs a Handler. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewHandler(store Store, connectedWithin time.Duration, metricsDBURL string, observationCadence time.Duration, dataStore *objectstore.Client, maxCheckpointGrace time.Duration, pricedAcceleratorTypes []string, maxLogLineChars int, signingSecret string, environments *EnvironmentRegistry, logger *zap.Logger) *Handler {
	priced := make(map[string]bool, len(pricedAcceleratorTypes))
	for _, name := range pricedAcceleratorTypes {
		priced[strings.ToLower(name)] = true
	}
	return &Handler{store: store, connectedWithin: connectedWithin, metricsDBURL: metricsDBURL, observationCadence: observationCadence,
		dataStore: dataStore, maxCheckpointGrace: maxCheckpointGrace,
		pricedAcceleratorTypes: priced, maxLogLineChars: maxLogLineChars, signingSecret: signingSecret, environments: environments, logger: logger}
}

// keepPricedAccelerators drops every accelerator type outside the priced catalog from report.
func (h *Handler) keepPricedAccelerators(report *capacityReport) {
	unpriced := func(flavor string) bool { return !h.pricedAcceleratorTypes[strings.ToLower(flavor)] }
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

// attachDataAccess hands each workload its durable-data address, derived from the experiment's
// own identity (platform experiment, agent, job), and the job's permanent key stored with it. The
// store already limits that key to writing the job's own prefix and reading its platform
// experiment's, so no agent can overwrite another's evidence whatever code it runs.
func (h *Handler) attachDataAccess(exps []*domain.Experiment) {
	for _, exp := range exps {
		exp.Data = &domain.DataAccess{
			URI:             h.dataStore.URI(objectstore.JobPrefix(exp.PlatformExperimentID, exp.AgentID, exp.ID)),
			Shared:          h.dataStore.URI(objectstore.PlatformExperimentPrefix(exp.PlatformExperimentID)),
			Endpoint:        h.dataStore.Endpoint,
			Region:          h.dataStore.Region,
			AccessKeyID:     exp.DataKey.AccessKeyID,
			SecretAccessKey: exp.DataKey.SecretAccessKey,
		}
	}
}

// attachTokens hands each workload the token its job reports metrics with. The token is derived
// from the job's id, so every reconcile answers with the same one.
func (h *Handler) attachTokens(exps []*domain.Experiment) error {
	for _, exp := range exps {
		token, err := auth.Mint(h.signingSecret, auth.JobScope(exp.ID))
		if err != nil {
			return err
		}
		exp.Token = token
	}
	return nil
}

// RegisterHuma registers the cluster-agent-facing operations on doc. Paths are relative to
// the /internal/clusters mount. Tagged "cluster-agent" — a separate consumer from the
// research agent, so it gets its own OpenAPI doc / explore.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "list-clusters", Method: "GET", Path: "/",
		Summary: "List clusters and their connectivity", Tags: []string{"cluster-agent"},
		Description: "Every cluster that has ever polled, and whether its agent is connected right now.",
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Clusters []clusterInfo `json:"clusters"`
		}
	}, error) {
		heartbeats, err := metricsdb.LiveClusterHeartbeats(ctx, h.metricsDBURL, h.connectedWithin)
		if err != nil {
			return nil, auth.Internal(h.logger, "list clusters", err)
		}
		available, total, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, h.metricsDBURL, h.connectedWithin)
		if err != nil {
			return nil, auth.Internal(h.logger, "list clusters", err)
		}
		clusterIDs, err := metricsdb.LiveClusterIDs(ctx, h.metricsDBURL, h.connectedWithin)
		if err != nil {
			return nil, auth.Internal(h.logger, "list clusters", err)
		}
		autoscalerEnabled, err := metricsdb.LiveClusterAutoscalerCapability(ctx, h.metricsDBURL, h.connectedWithin)
		if err != nil {
			return nil, auth.Internal(h.logger, "list clusters", err)
		}
		names := make([]string, 0, len(heartbeats))
		for name := range heartbeats {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]clusterInfo, len(names))
		now := time.Now()
		for i, name := range names {
			connected := heartbeats[name]
			var lastSeen time.Time
			if connected {
				lastSeen = now
			}
			devices, cores := occupancyByKind(available[name], total[name])
			out[i] = clusterInfo{
				ClusterName:       name,
				ClusterID:         clusterIDs[name],
				LastSeenAt:        lastSeen,
				Connected:         connected,
				AcceleratorBusy:   devices.busy,
				AcceleratorTotal:  devices.total,
				CoreBusy:          cores.busy,
				CoreTotal:         cores.total,
				AutoscalerEnabled: autoscalerEnabled[name],
			}
		}
		resp := &struct {
			Body struct {
				Clusters []clusterInfo `json:"clusters"`
			}
		}{}
		resp.Body.Clusters = out
		return resp, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-reconcile", Method: "POST", Path: "/{name}/reconcile",
		Summary: "Exchange actual capacity for desired workloads", Tags: []string{"cluster-agent"},
		Description: "Atomically records one complete actual-capacity snapshot in metrics storage, keeping only priced accelerator types, then returns the complete PostgreSQL desired workload set for this cluster, plus the ids of workloads whose termination earns them their declared checkpoint window, " +
			"the environment builds assigned to this cluster (builds, recorded as building only for a runtime that reports builds true), and registry_pull, a login that reads environment images, when a listed workload runs in one.",
	}, func(ctx context.Context, in *reconcileInput) (*struct {
		Body reconcileBody
	}, error) {
		clusterName := in.Name

		report := in.Body
		if report.AcceleratorAvailableByFlavor == nil || report.AcceleratorTotalByFlavor == nil || report.AcceleratorAvailableByNode == nil {
			return nil, huma.Error400BadRequest("complete accelerator capacity report is required")
		}
		h.keepPricedAccelerators(&report)
		if report.NodeResourcesByNode == nil {
			return nil, huma.Error400BadRequest("per-node resource capacity report is required")
		}
		if report.NodeResourcesTotalByNode == nil {
			return nil, huma.Error400BadRequest("per-node total resource capacity report is required")
		}
		for node, byResource := range report.NodeResourcesByNode {
			if node == "" {
				return nil, huma.Error400BadRequest("per-node resource report has empty node identity")
			}
			for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
				if _, present := byResource[key]; !present {
					return nil, huma.Error400BadRequest("per-node resource report for " + node + " is missing " + key)
				}
			}
			for resource, available := range byResource {
				if resource == "" || available < 0 {
					return nil, huma.Error400BadRequest("invalid per-node resource capacity value")
				}
			}
			total, present := report.NodeResourcesTotalByNode[node]
			if !present {
				return nil, huma.Error400BadRequest("per-node total resource report for " + node + " is missing")
			}
			for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
				totalValue, present := total[key]
				if !present || totalValue < 0 {
					return nil, huma.Error400BadRequest("per-node total resource report for " + node + " is missing or invalid " + key)
				}
				if byResource[key] > totalValue {
					return nil, huma.Error400BadRequest("per-node available resource exceeds total for " + node)
				}
			}
		}
		if report.CPUAvailableCores < 0 || report.CPUTotalCores < 0 || report.CPUAvailableCores > report.CPUTotalCores {
			return nil, huma.Error400BadRequest("invalid CPU capacity values")
		}
		for flavor, total := range report.AcceleratorTotalByFlavor {
			if flavor == "" {
				return nil, huma.Error400BadRequest("accelerator capacity contains empty flavor")
			}
			available, present := report.AcceleratorAvailableByFlavor[flavor]
			if !present || total < 0 || available < 0 || available > total {
				return nil, huma.Error400BadRequest("invalid accelerator capacity values")
			}
		}
		if len(report.AcceleratorAvailableByFlavor) != len(report.AcceleratorTotalByFlavor) {
			return nil, huma.Error400BadRequest("accelerator available and total flavor sets must match")
		}
		availableByNode := make(map[string]int64, len(report.AcceleratorAvailableByFlavor))
		for node, flavors := range report.AcceleratorAvailableByNode {
			if node == "" {
				return nil, huma.Error400BadRequest("per-node accelerator report has empty node identity")
			}
			for flavor, available := range flavors {
				if flavor == "" || available < 0 {
					return nil, huma.Error400BadRequest("invalid per-node accelerator capacity value")
				}
				if _, present := report.AcceleratorTotalByFlavor[flavor]; !present {
					return nil, huma.Error400BadRequest("per-node accelerator flavor is absent from cluster totals")
				}
				availableByNode[flavor] += available
			}
		}
		for flavor, byNode := range availableByNode {
			if byNode > report.AcceleratorAvailableByFlavor[flavor] {
				return nil, huma.Error400BadRequest("per-node accelerator capacity exceeds cluster availability")
			}
		}

		if report.RAMAvailableBytes < 0 || report.RAMTotalBytes < 0 || report.RAMAvailableBytes > report.RAMTotalBytes ||
			report.StorageAvailableBytes < 0 || report.StorageTotalBytes < 0 || report.StorageAvailableBytes > report.StorageTotalBytes {
			return nil, huma.Error400BadRequest("invalid memory or storage capacity values")
		}

		// The scheduler knows a connected cluster by the id its runtime reports, so an id is one cluster's and
		// a cluster has one. A runtime stating another is refused before anything it reports is
		// recorded.
		if report.ClusterID != "" {
			if err := domain.ValidateClusterID(report.ClusterID); err != nil {
				return nil, huma.Error400BadRequest(err.Error())
			}
			switch err := h.store.BindReportedID(ctx, clusterName, report.ClusterID); {
			case errors.Is(err, db.ErrClusterNotFound):
				return nil, auth.NotFound("cluster")
			case errors.Is(err, db.ErrClusterIDBound):
				return nil, huma.Error409Conflict("cluster_id " + report.ClusterID + " is the identity of another registered cluster")
			case errors.Is(err, db.ErrClusterIDChanged):
				return nil, huma.Error409Conflict("cluster " + clusterName + " is bound to another cluster_id than the " + report.ClusterID +
					" this runtime reports: if its machines were rebuilt, its owner forgets the old identity (hl cluster forget-identity " + clusterName + ")")
			case err != nil:
				return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
			}
		}

		// Placement reads the image, so it is kept before the capacity that makes the cluster a
		// candidate.
		if len(report.JobImage) > domain.MaxJobImageLength {
			return nil, huma.Error400BadRequest("job_image is longer than " + strconv.Itoa(domain.MaxJobImageLength) + " characters")
		}
		onlyImage := report.JobImage
		if report.JobImageDefault {
			onlyImage = ""
		}
		if err := h.store.RecordJobImage(ctx, clusterName, onlyImage); err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
		if err := h.store.RecordBuilds(ctx, clusterName, report.Builds && h.environments != nil); err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}

		if err := metricsdb.RecordClusterCapacitySnapshot(ctx, h.metricsDBURL, metricsdb.ClusterCapacitySnapshot{
			ClusterName: clusterName, At: time.Now().UTC(),
			CPUAvailable: report.CPUAvailableCores, CPUTotal: report.CPUTotalCores,
			AcceleratorAvailable: report.AcceleratorAvailableByFlavor, AcceleratorTotal: report.AcceleratorTotalByFlavor,
			AcceleratorAvailableByNode: report.AcceleratorAvailableByNode,
			NodeResourcesByNode:        report.NodeResourcesByNode,
			NodeResourcesTotalByNode:   report.NodeResourcesTotalByNode,
			NodeLabelsByNode:           report.NodeLabelsByNode,
			MultiNodeCapable:           report.MultiNodeCapable,
			AutoscalerEnabled:          report.AutoscalerEnabled,
			ClusterID:                  report.ClusterID,
			RAMAvailable:               report.RAMAvailableBytes, RAMTotal: report.RAMTotalBytes,
			StorageAvailable: report.StorageAvailableBytes, StorageTotal: report.StorageTotalBytes,
		}); err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}

		// Each job says when its instance ends at the latest: the close of its platform experiment,
		// the wind-down a close is given and the longest checkpoint window after it.
		exps, err := h.store.ListDesiredWorkloads(ctx, clusterName, domain.AgentWindDown+h.maxCheckpointGrace)
		if err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
		// The other half of the same view: what should NO LONGER exist, and for how much longer
		// it may. A job the platform itself decided to stop -- preempted, cut, out of quota, out
		// of time -- is fine and its work is worth saving, so it is told termination is coming
		// and gets the window it declared before it is killed. A job the environment or its own
		// code ended gets no window; it simply never appears here.
		//
		// Derived from the termination that is already recorded on the experiment, and expiring
		// with maxCheckpointGrace, so there is no terminating state to write, advance or clear.
		undesired, err := h.store.ListRecentlyUndesiredWorkloads(ctx, clusterName, h.maxCheckpointGrace)
		if err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
		resp := &struct {
			Body reconcileBody
		}{}
		h.attachDataAccess(exps)
		if err := h.attachTokens(exps); err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
		// A runtime may belong to another org than the job, so it is handed what it builds a
		// workload from and nothing else of the job.
		resp.Body.Experiments = make([]*domain.Experiment, len(exps))
		for i, exp := range exps {
			resp.Body.Experiments[i] = exp.ForRuntime()
		}
		resp.Body.CheckpointWindow = domain.CheckpointWindowGrants(undesired)
		if resp.Body.Builds, resp.Body.RegistryPull, err = h.environmentWork(ctx, clusterName, exps); err != nil {
			return nil, auth.Internal(h.logger, "cluster reconcile", err, zap.String("cluster", clusterName))
		}
		return resp, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-credential", Method: "POST", Path: "/{name}/builds/{key}/credential",
		Summary: "The push login of an assigned environment build", Tags: []string{"cluster-agent"},
		Description: "A login that writes to the environment repository for at least 25 minutes, handed out only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Key  string `path:"key"`
		Body domain.EnvironmentBuildCredentialRequest
	}) (*struct {
		Body domain.RegistryLogin
	}, error) {
		if h.environments == nil {
			return nil, huma.Error409Conflict("this deployment builds no environments")
		}
		env, err := h.store.AssignedEnvironmentBuild(ctx, in.Name, in.Key, in.Body.Attempt)
		if err != nil {
			return nil, auth.Internal(h.logger, "environment build credential", err, zap.String("cluster", in.Name), zap.String("key", in.Key))
		}
		if env == nil {
			return nil, huma.Error409Conflict("the build of " + in.Key + " is not assigned to cluster " + in.Name + " at attempt " + strconv.Itoa(in.Body.Attempt))
		}
		login, err := h.environments.Logins.Push(ctx)
		if err != nil {
			return nil, auth.Internal(h.logger, "environment build credential", err, zap.String("cluster", in.Name), zap.String("key", in.Key))
		}
		return &struct {
			Body domain.RegistryLogin
		}{Body: login}, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-report", Method: "POST", Path: "/{name}/builds/{key}",
		Summary: "Report the outcome of an assigned environment build", Tags: []string{"cluster-agent"},
		Description: "outcome pushed makes the environment ready in the image the repository holds under digest; file fails it with log_tail as the file's fault; " +
			"platform frees the build to be assigned again. Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Key  string `path:"key"`
		Body domain.EnvironmentBuildReport
	}) (*struct{}, error) {
		if h.environments == nil {
			return nil, huma.Error409Conflict("this deployment builds no environments")
		}
		report := in.Body
		if len(report.LogTail) > domain.MaxEnvironmentLogBytes {
			return nil, huma.Error400BadRequest("log_tail is longer than " + strconv.Itoa(domain.MaxEnvironmentLogBytes) + " bytes")
		}
		outcome := domain.EnvironmentBuildOutcome{LogTail: report.LogTail}
		switch report.Outcome {
		case domain.EnvironmentBuildPushed:
			parsed, err := digest.Parse(report.Digest)
			if err != nil || parsed.Algorithm() != digest.SHA256 {
				return nil, huma.Error400BadRequest("a pushed build names its digest, sha256:<hex>")
			}
			outcome.Image = h.environments.Repository + "@" + report.Digest
		case domain.EnvironmentBuildFileFault:
			outcome.Fault = domain.EnvironmentFaultFile
		case domain.EnvironmentBuildPlatformFault:
			outcome.Fault = domain.EnvironmentFaultPlatform
		default:
			return nil, huma.Error400BadRequest("outcome is pushed, file or platform")
		}
		if report.Outcome != domain.EnvironmentBuildPushed && report.Digest != "" {
			return nil, huma.Error400BadRequest("only a pushed build names a digest")
		}
		current, err := h.store.FinishEnvironmentBuild(ctx, in.Name, in.Key, report.Attempt, outcome)
		if err != nil {
			return nil, auth.Internal(h.logger, "environment build report", err, zap.String("cluster", in.Name), zap.String("key", in.Key))
		}
		if !current {
			return nil, huma.Error409Conflict("the build of " + in.Key + " is not assigned to cluster " + in.Name + " at attempt " + strconv.Itoa(report.Attempt))
		}
		h.logger.Info("environment build reported", zap.String("cluster", in.Name), zap.String("key", in.Key),
			zap.Int("attempt", report.Attempt), zap.String("outcome", string(report.Outcome)))
		h.wakeAdmission()
		return nil, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-loaded", Method: "POST", Path: "/{name}/builds/{key}/loaded",
		Summary: "Report an assigned environment build held on one node", Tags: []string{"cluster-agent"},
		Description: "The image of the build, the repository's under digest, is in the image store of node while its push goes on: a job that waits for the environment and fits on that node is placed there, pinned to it, " +
			"and runs the image from there before it is pushed. The outcome is still reported once the push ends. " +
			"Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Key  string `path:"key"`
		Body domain.EnvironmentBuildLoaded
	}) (*struct{}, error) {
		if h.environments == nil {
			return nil, huma.Error409Conflict("this deployment builds no environments")
		}
		loaded := in.Body
		if parsed, err := digest.Parse(loaded.Digest); err != nil || parsed.Algorithm() != digest.SHA256 {
			return nil, huma.Error400BadRequest("digest is the image's, sha256:<hex>")
		}
		if loaded.Node == "" || len(loaded.Node) > domain.MaxNodeNameBytes || strings.ContainsFunc(loaded.Node, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return nil, huma.Error400BadRequest("node is the name of the node, 1 to " + strconv.Itoa(domain.MaxNodeNameBytes) + " bytes")
		}
		current, err := h.store.RecordEnvironmentLoaded(ctx, in.Name, in.Key, loaded.Attempt, loaded.Node, h.environments.Repository+"@"+loaded.Digest)
		if err != nil {
			return nil, auth.Internal(h.logger, "environment build loaded", err, zap.String("cluster", in.Name), zap.String("key", in.Key))
		}
		if !current {
			return nil, huma.Error409Conflict("the build of " + in.Key + " is not assigned to cluster " + in.Name + " at attempt " + strconv.Itoa(loaded.Attempt))
		}
		h.logger.Info("environment build loaded", zap.String("cluster", in.Name), zap.String("key", in.Key),
			zap.Int("attempt", loaded.Attempt), zap.String("node", loaded.Node), zap.String("digest", loaded.Digest))
		h.wakeAdmission()
		return nil, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-progress", Method: "POST", Path: "/{name}/builds/{key}/progress",
		Summary: "Report the step an assigned environment build is at", Tags: []string{"cluster-agent"},
		Description: "step, one line of at most " + strconv.Itoa(domain.MaxEnvironmentProgressBytes) + " bytes, replaces the last one reported, and the jobs waiting for the environment show it. " +
			"Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Key  string `path:"key"`
		Body domain.EnvironmentBuildProgress
	}) (*struct{}, error) {
		if h.environments == nil {
			return nil, huma.Error409Conflict("this deployment builds no environments")
		}
		step := in.Body.Step
		if step == "" || len(step) > domain.MaxEnvironmentProgressBytes || strings.ContainsAny(step, "\r\n") {
			return nil, huma.Error400BadRequest("step is one line of 1 to " + strconv.Itoa(domain.MaxEnvironmentProgressBytes) + " bytes")
		}
		current, err := h.store.RecordEnvironmentProgress(ctx, in.Name, in.Key, in.Body.Attempt, step)
		if err != nil {
			return nil, auth.Internal(h.logger, "environment build progress", err, zap.String("cluster", in.Name), zap.String("key", in.Key))
		}
		if !current {
			return nil, huma.Error409Conflict("the build of " + in.Key + " is not assigned to cluster " + in.Name + " at attempt " + strconv.Itoa(in.Body.Attempt))
		}
		return nil, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-detach", Method: "DELETE", Path: "/{name}",
		Summary: "Detach this cluster", Tags: []string{"cluster-agent"},
		Description: "The cluster's own runtime ends the cluster, as its owner does with DELETE /orgs/{id}/clusters/{name}: every job placed on it goes back to the queue, " +
			"its settings and the identity bound to it are deleted, and its runtime token is refused from then on with reason cluster_not_registered. requeued names the jobs.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
	}) (*struct {
		Body struct {
			Requeued []string `json:"requeued"`
		}
	}, error) {
		requeued, err := h.store.RemoveCluster(ctx, in.Name)
		switch {
		case errors.Is(err, db.ErrClusterNotFound):
			return nil, auth.NotFound("cluster")
		case err != nil:
			return nil, auth.Internal(h.logger, "detach cluster", err, zap.String("cluster", in.Name))
		}
		h.logger.Info("cluster detached by its runtime", zap.String("cluster", in.Name), zap.Int("requeued", len(requeued)))
		resp := &struct {
			Body struct {
				Requeued []string `json:"requeued"`
			}
		}{}
		resp.Body.Requeued = append([]string{}, requeued...)
		return resp, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-push-status", Method: "POST", Path: "/{name}/status",
		Summary: "Push job-phase observations from a cluster-agent", Tags: []string{"cluster-agent"},
		Description: "Body: {\"reports\": [...]}. The complete snapshot is validated and written atomically; any invalid report rejects the whole request.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Body struct {
			Reports []statusReport `json:"reports"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		clusterName := in.Name
		now := time.Now().UTC()
		statusSamples := make([]metricsdb.JobStatusSample, 0, len(in.Body.Reports))
		// A runtime reporting a job as running is an observation that it is alive, and it is the
		// one liveness observation every runtime produces — the per-pod cgroup heartbeat exists
		// only where a node-agent is deployed. Recording it here, once, is what gives runtimes
		// without one (bare metal) any observed runtime at all: without it their jobs were billed
		// only in slivers around whatever metrics they happened to report, their preemption
		// rescale computed a stint of ~0 and requeued them at full estimate, and every rule keyed
		// on "never observed" could not be bounded because it would have condemned all of them.
		var observations []metricsdb.Observation
		ids := make([]string, 0, len(in.Body.Reports))
		for _, rep := range in.Body.Reports {
			if rep.ExperimentID == "" || rep.Phase == "" {
				return nil, huma.Error400BadRequest("every status report requires experiment_id and phase")
			}
			if rep.AdmittedMachine != nil && rep.AdmittedNode == "" {
				return nil, huma.Error400BadRequest("status report for " + rep.ExperimentID + " names an admitted_machine and no admitted_node")
			}
			if (rep.StartedAt == nil) != (rep.EndedAt == nil) || (rep.StartedAt != nil && rep.EndedAt.Before(*rep.StartedAt)) {
				return nil, huma.Error400BadRequest("status report for " + rep.ExperimentID + " needs started_at and ended_at together, in that order")
			}
			ids = append(ids, rep.ExperimentID)
		}
		placementByID, err := h.store.PlacementByID(ctx, ids)
		if err != nil {
			return nil, auth.Internal(h.logger, "cluster push status", err, zap.String("cluster", clusterName))
		}
		accepted := make([]statusReport, 0, len(in.Body.Reports))
		var ended []string
		for _, rep := range in.Body.Reports {
			// A report for an experiment this cluster no longer owns is dropped, not a reason to
			// reject the push. Mid-reschedule that is the ordinary state — the experiment has
			// already been repointed at its new cluster while the old one is still tearing its
			// workload down — and rejecting the whole snapshot blacked out status for every other
			// job on the old cluster until it finished. Dropping it is also the honest report:
			// the experiment genuinely is not this cluster's any more.
			if reason, ok := rejectReport(rep, placementByID[rep.ExperimentID], clusterName); !ok {
				h.logger.Info(reason, zap.String("experiment", rep.ExperimentID),
					zap.String("cluster", clusterName), zap.Int("reported_attempt", reportedAttempt(rep)),
					zap.Int("current_attempt", placementByID[rep.ExperimentID].AttemptCount))
				continue
			}
			accepted = append(accepted, rep)
			if rep.EndedAt != nil {
				ended = append(ended, rep.ExperimentID)
			}
		}
		// A runtime reports an ended run on every push until its workload is deleted. The run is
		// placed by the time of the first of them, so the later ones write the same observations.
		runReportedAt, err := h.store.RunFirstReportedAt(ctx, ended)
		if err != nil {
			return nil, auth.Internal(h.logger, "cluster push status", err, zap.String("cluster", clusterName))
		}
		for _, rep := range accepted {
			statusSamples = append(statusSamples, metricsdb.JobStatusSample{
				ExperimentID: rep.ExperimentID, ClusterName: clusterName, Phase: rep.Phase,
				AdmittedAcceleratorType: rep.AdmittedAcceleratorType, AdmittedNode: rep.AdmittedNode, AdmittedMachine: rep.AdmittedMachine, At: now,
			})
			observations = append(observations, observationsOf(rep, placementByID[rep.ExperimentID].AdmittedAt, runReportedAt[rep.ExperimentID], clusterName, now, h.observationCadence)...)
			// LogTail is optional per report (nil when the executor has nothing new)
			// -- only write when the agent actually sent one, so a
			// job with no fresh output this tick doesn't wipe out its last-known tail.
			//
			// A tail or a phase detail that cannot be written fails the push before any phase is
			// recorded: the phase that ends a job is what its workload is deleted on, and the
			// reason it ended would be gone with it. The runtime pushes the snapshot again.
			if rep.LogTail != nil {
				if err := metricsdb.RecordLogTail(ctx, h.metricsDBURL, rep.ExperimentID, clusterName, splitLongLines(rep.LogTail, h.maxLogLineChars), now); err != nil {
					return nil, auth.Internal(h.logger, "cluster push status", err, zap.String("experiment_id", rep.ExperimentID), zap.String("cluster", clusterName))
				}
			}
			// Reason/Message/RestartCount are optional the same way: only write when the
			// runtime actually has something to say about why this job's container hasn't
			// started or has been restarting, so a healthy tick doesn't overwrite a still-valid
			// prior explanation with silence.
			if rep.Reason != "" || rep.Message != "" || rep.RestartCount != 0 || rep.ScheduledNodes != 0 || rep.SchedulingReason != "" {
				if err := metricsdb.RecordPhaseDetail(ctx, h.metricsDBURL, rep.ExperimentID, clusterName, rep.Reason, rep.Message, rep.RestartCount, rep.ScheduledNodes, rep.SchedulingReason, placementByID[rep.ExperimentID].AttemptCount, now); err != nil {
					return nil, auth.Internal(h.logger, "cluster push status", err, zap.String("experiment_id", rep.ExperimentID), zap.String("cluster", clusterName))
				}
			}
		}
		// Observations first: the job watcher settles a job as soon as it reads the phase that
		// ended it, from whatever was observed of it by then.
		if len(observations) > 0 {
			if err := metricsdb.RecordObservations(ctx, h.metricsDBURL, observations); err != nil {
				return nil, auth.Internal(h.logger, "cluster push status", err, zap.String("cluster", clusterName))
			}
		}
		if err := metricsdb.RecordJobStatuses(ctx, h.metricsDBURL, clusterName, now, statusSamples); err != nil {
			return nil, auth.Internal(h.logger, "cluster push status", err, zap.String("cluster", clusterName))
		}
		resp := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		resp.Body.Status = "ok"
		return resp, nil
	})
}

// environmentWork is the builds assigned to cluster, and the pull login when one of exps, the
// workloads desired on it, runs in an environment.
func (h *Handler) environmentWork(ctx context.Context, cluster string, exps []*domain.Experiment) ([]domain.EnvironmentBuild, *domain.RegistryLogin, error) {
	builds := []domain.EnvironmentBuild{}
	if h.environments == nil {
		return builds, nil, nil
	}
	assigned, err := h.store.AssignedEnvironmentBuilds(ctx, cluster)
	if err != nil {
		return nil, nil, err
	}
	for _, env := range assigned {
		builds = append(builds, domain.EnvironmentBuild{Key: env.Key, Attempt: env.Attempt, Base: env.Base, Dockerfile: env.Dockerfile, Repository: h.environments.Repository})
	}
	if !slices.ContainsFunc(exps, func(exp *domain.Experiment) bool { return exp.EnvironmentKey != "" }) {
		return builds, nil, nil
	}
	// Without a login only a pull fails, and a desired state that is refused instead has the
	// runtime act on none of it.
	login, err := h.environments.Logins.Pull(ctx)
	if err != nil {
		h.logger.Error("cluster reconcile: mint registry pull login", zap.String("cluster", cluster), zap.Error(err))
		return builds, nil, nil
	}
	return builds, &login, nil
}

// reconcileBody is one reconcile pass's whole answer: what should exist here, and which of the
// things that should no longer exist are entitled to their checkpoint window on the way out.
type reconcileBody struct {
	Experiments []*domain.Experiment `json:"experiments"`
	// CheckpointWindow names the experiments whose termination was the platform's own decision.
	// Deliberately ids and nothing else: the runtime is told which workloads still have a window
	// coming, never the fault class or the rule that produced it. How long each window is was
	// declared on the job's own spec and compiled into the workload when the runtime created it.
	CheckpointWindow []string `json:"checkpoint_window"`
	// Builds are the environment builds assigned to this cluster now.
	Builds []domain.EnvironmentBuild `json:"builds"`
	// RegistryPull pulls the environment images of the experiments listed, sent only when one
	// of them runs in one.
	RegistryPull *domain.RegistryLogin `json:"registry_pull,omitempty"`
}

type reconcileInput struct {
	Name string `path:"name"`
	Body capacityReport
}

type capacityReport struct {
	CPUAvailableCores            float64                     `json:"cpu_available_cores"`
	CPUTotalCores                float64                     `json:"cpu_total_cores"`
	AcceleratorAvailableByFlavor map[string]int64            `json:"accelerator_available_by_type"`
	AcceleratorTotalByFlavor     map[string]int64            `json:"accelerator_total_by_type"`
	AcceleratorAvailableByNode   map[string]map[string]int64 `json:"accelerator_available_by_node"`
	// NodeResourcesByNode is free CPU/memory/storage per node, keyed by domain.NodeResource*.
	// Required: a job runs on one node and must fit that node in every dimension, and a
	// cluster-wide total cannot answer that — see scheduler.reservePlacement.
	NodeResourcesByNode map[string]map[string]int64 `json:"node_resources_by_node"`
	// NodeResourcesTotalByNode mirrors NodeResourcesByNode but carries each node's installed
	// (allocatable) amount rather than what's currently free — the stable per-node denominator
	// fair-share math needs, since free capacity fluctuates with what's running. Required for the
	// same reason NodeResourcesByNode is: a job's node-local fair share cannot be judged against a
	// number that moves every time something else is scheduled or freed.
	NodeResourcesTotalByNode map[string]map[string]int64  `json:"node_resources_total_by_node"`
	NodeLabelsByNode         map[string]map[string]string `json:"node_labels_by_node"`
	// MultiNodeCapable is whether this cluster's runtime can execute a job spanning more than one
	// node. A capability of the cluster, reported alongside its capacity and read by admission —
	// see agentexec.Executor.SupportsMultiNodeJobs. Absent (false) means single-node only, which
	// is the safe reading: a cluster that has not said it can run distributed work does not get
	// distributed work.
	MultiNodeCapable bool `json:"multi_node_capable"`
	// JobImage is the one image this cluster runs, for a runtime that boots its own
	// (agentexec.Executor.JobImage). Absent means it runs whatever image a job names.
	// Placement reads it: a job naming no image goes only where one is fixed, and a job naming
	// one only where that image runs.
	JobImage string `json:"job_image,omitempty"`
	// JobImageDefault is sent only by a runtime built when a cluster that runs any image also
	// reported, in job_image, where a job naming none ran. True, the cluster runs any image and
	// job_image is not read. It goes once no such runtime is installed: a report that carries
	// a field this body does not name is refused.
	JobImageDefault bool `json:"job_image_default,omitempty"`
	// AutoscalerEnabled is whether this cluster sits behind a native autoscaler (cluster-autoscaler
	// / Karpenter) that reacts to Pending pods. Operator-set on the agent deployment
	// (AUTOSCALER_ENABLED), fail-closed: absent (false) means the scheduler never speculatively
	// submits onto this cluster.
	AutoscalerEnabled bool `json:"autoscaler_enabled"`
	// Builds is whether this cluster's runtime builds environments. Absent (false) means it is
	// assigned none.
	Builds bool `json:"builds,omitempty"`
	// ClusterID is a stable fingerprint the runtime derives live and never persists itself: the
	// kube-system namespace UID on k8s, /etc/machine-id on bare metal. Unlike ClusterName (an
	// operator-typed label that can be renamed or duplicated), this identifies the cluster across
	// a rename — everything scheduler-side that must survive a rename (the cluster's settings,
	// tried_clusters) keys on it instead of the name.
	ClusterID             string `json:"cluster_id"`
	RAMAvailableBytes     int64  `json:"ram_available_bytes"`
	RAMTotalBytes         int64  `json:"ram_total_bytes"`
	StorageAvailableBytes int64  `json:"storage_available_bytes"`
	StorageTotalBytes     int64  `json:"storage_total_bytes"`
}

// clusterInfo is one row of GET /internal/clusters.
type occupancy struct{ busy, total int64 }

// occupancyByKind sums one cluster's reported flavors into devices and cores. The CPU type counts
// cores and an accelerator type devices, so the two are never one sum.
func occupancyByKind(available, total map[string]int64) (devices, cores occupancy) {
	for flavor, installed := range total {
		sum := &devices
		if domain.AcceleratorType(flavor).Kind() == domain.TypeKindCPU {
			sum = &cores
		}
		sum.total += installed
		sum.busy += installed - available[flavor]
	}
	return devices, cores
}

type clusterInfo struct {
	ClusterName string `json:"cluster_name"`
	// ClusterID is the runtime-derived stable fingerprint (kube-system namespace UID /
	// machine-id), empty for a cluster whose agent has not reported it (older runtime build, or
	// no heartbeat within the freshness window).
	ClusterID  string    `json:"cluster_id"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Connected  bool      `json:"connected"`
	// AcceleratorBusy/AcceleratorTotal are the cluster's most recently reported occupancy,
	// summed across every flavor of kind accelerator — actual busy-vs-idle chip counts from the
	// last reconcile snapshot, not a budget-consumption ratio. Zero/absent when no live snapshot
	// is within the connectedWithin freshness window (e.g. a disconnected cluster).
	AcceleratorBusy  int64 `json:"accelerator_busy"`
	AcceleratorTotal int64 `json:"accelerator_total"`
	// CoreBusy/CoreTotal are the same for the cores the cluster offers to jobs on CPU. Kept
	// apart: 16 cores and 4 chips are not 20 of anything.
	CoreBusy  int64 `json:"core_busy"`
	CoreTotal int64 `json:"core_total"`
	// AutoscalerEnabled mirrors the cluster's own most recent capacityReport — see
	// capacityReport.AutoscalerEnabled. False (the fail-closed default) for a cluster whose agent
	// has never reported it or is not within the freshness window.
	AutoscalerEnabled bool `json:"autoscaler_enabled"`
}

// statusReport is one job-status push from a cluster-agent. Reason/Message/RestartCount are the
// runtime's explanation for why a job's container hasn't started (or has been restarting) — see
// domain.PhaseDetail. Runtime-agnostic strings: each runtime (k8s, bare-metal) translates its
// own native vocabulary into domain.PhaseReason* before this ever leaves the agent process.
type statusReport struct {
	ExperimentID            string `json:"experiment_id"`
	Phase                   string `json:"phase"` // pending | running | succeeded | failed | gone
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string `json:"admitted_node,omitempty"`
	// AdmittedMachine is what stands behind AdmittedNode, from a runtime that knows it. It goes
	// only with a node. Absent from a runtime built before it, and from one that knows no more
	// than the node.
	AdmittedMachine *domain.JobMachine `json:"admitted_machine,omitempty"`
	LogTail         []string           `json:"log_tail,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	Message         string             `json:"message,omitempty"`
	RestartCount    int32              `json:"restart_count,omitempty"`
	// ScheduledNodes is the count of this report's pods with condition PodScheduled=True — see
	// PhaseDetailRow.ScheduledNodes. Absent (0) on runtimes that don't yet compute it or for a
	// report where nothing has bound.
	ScheduledNodes int32 `json:"scheduled_nodes,omitempty"`
	// SchedulingReason is a best-effort explanation for a still-Pending pod (autoscaler event
	// message) — see PhaseDetailRow.SchedulingReason.
	SchedulingReason string `json:"scheduling_reason,omitempty"`
	// Attempt is the generation of the workload this observation came from, as the control plane
	// numbered it (domain.Experiment.AttemptCount, handed to the runtime and carried on the
	// workload it created). A pointer because absent and zero are different answers — see
	// reportedAttempt.
	Attempt *int `json:"attempt,omitempty"`
	// StartedAt and EndedAt are when the job's command started and ended, from a runtime whose
	// own look at the workload cannot time it: one that rents machines sees them through a
	// vendor, and reads the times the wrapper beside the command measured. Sent together, once
	// the command has ended. The time between them is the job's observed run (measuredRun).
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// measuredRun places the run a machine timed by its own clock on this one. How long the command
// ran is the machine's word, since a clock that is wrong is wrong at both ends alike. When it ran
// is held between the job's admission and the first report of its end, which is all the platform
// saw of it: a run told by a clock that is ahead ends at that report, one told by a clock that is
// behind starts at the admission, and none is longer than the time between the two. Both bounds
// are stored, so every report of one run places it the same.
func measuredRun(startedAt, endedAt, admittedAt, reportedAt time.Time) (from, to time.Time) {
	ran := min(endedAt.Sub(startedAt), reportedAt.Sub(admittedAt))
	to = endedAt
	if to.After(reportedAt) {
		to = reportedAt
	}
	if to.Add(-ran).Before(admittedAt) {
		to = admittedAt.Add(ran)
	}
	return to.Add(-ran), to
}

// observationsOf is when rep shows its job alive: now for a job reported running, and the whole
// of the run its runtime measured, as observations no further apart than cadence, so that the
// span every reader computes (metricsdb.ObserveSpan) holds the run whole however rarely the job
// was seen running. A run reported for a job that holds no capacity is nobody's. runReportedAt is
// when the end of the run was first reported (Store.RunFirstReportedAt).
func observationsOf(rep statusReport, admittedAt, runReportedAt time.Time, clusterName string, now time.Time, cadence time.Duration) []metricsdb.Observation {
	labels := map[string]string{"cluster_name": clusterName}
	if rep.AdmittedNode != "" {
		labels["node"] = rep.AdmittedNode
	}
	at := func(t time.Time) metricsdb.Observation {
		return metricsdb.Observation{ExperimentID: rep.ExperimentID, At: t.UTC(), ExtraLabels: labels}
	}
	var observations []metricsdb.Observation
	if rep.Phase == metricsdb.PhaseRunning {
		observations = append(observations, at(now))
	}
	if rep.EndedAt == nil || admittedAt.IsZero() || !runReportedAt.After(admittedAt) {
		return observations
	}
	from, to := measuredRun(*rep.StartedAt, *rep.EndedAt, admittedAt, runReportedAt)
	for t := from; t.Before(to); t = t.Add(cadence) {
		observations = append(observations, at(t))
	}
	return append(observations, at(to))
}

// rejectReport decides whether one status report describes the workload the control plane is
// actually waiting on. ok=false means drop it — never reject the whole push: mid-reschedule a
// stale report is the ordinary state, and failing the snapshot blacks out status for every other
// job on the cluster until it settles.
//
// Two things have to match. Ownership, because the experiment may already have been repointed at
// another cluster while this one is still tearing its workload down. And generation, because
// ownership alone does not cover a retry that lands back on the cluster it just failed on: the
// previous attempt's workload is still terminating and still in this snapshot
// (ListManagedJobsForStatus includes terminating workloads on purpose), so the Failed that caused
// the requeue would be recorded as the observation of the attempt that just started, and fail it
// again immediately.
//
// AttemptUnknown is accepted rather than dropped. Every cluster-agent sends it for a job it holds
// no workload for (phase gone), which has no generation to name, so absence must survive decoding
// as absence rather than as attempt 0.
//
// A job admitted to a slot has a third thing to match, the slot. A preempted job keeps its
// attempt, so readmitted to another slot of the cluster it was preempted on, the workload it left
// behind is of the right cluster and the right generation, and its exit would be recorded as the
// readmitted job's result. A report naming no node is accepted: it describes no workload in
// particular.
func rejectReport(rep statusReport, placement domain.Placement, clusterName string) (string, bool) {
	if placement.ClusterName != clusterName {
		return "dropping status report for an experiment this cluster no longer owns", false
	}
	if attempt := reportedAttempt(rep); attempt != workload.AttemptUnknown && attempt != placement.AttemptCount {
		return "dropping status report from a superseded attempt", false
	}
	if placement.Slot != "" && rep.AdmittedNode != "" && rep.AdmittedNode != placement.Slot {
		return "dropping status report from a slot the experiment is no longer placed on", false
	}
	return "", true
}

// reportedAttempt is the generation a report belongs to, or workload.AttemptUnknown when the
// cluster-agent named none.
func reportedAttempt(rep statusReport) int {
	if rep.Attempt == nil {
		return workload.AttemptUnknown
	}
	return *rep.Attempt
}
