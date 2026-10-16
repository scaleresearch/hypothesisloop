// Package agentloop is the backend-agnostic half of cluster-agent and bare-agent: fetch desired
// state from the control plane, diff it against whatever agentexec.Executor reports as actual,
// converge, and push status back.
package agentloop

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

// DefaultLogTailLines is how many trailing lines FetchLogTail is asked for when the agent
// binary doesn't override it (see OptionalIntEnv("LOG_TAIL_LINES", ...) in each cmd/'s main.go).
const DefaultLogTailLines = 100

// finalStatusCaptureBudget bounds the whole last-chance observation-and-push phase for workloads
// about to be deleted — the entire phase, not each workload, so a slow or unreachable control
// plane costs the pass this much once rather than once per workload. Deleting them is the pass's
// actual job; capturing why they ended is worth a few seconds of it and no more.
const finalStatusCaptureBudget = 10 * time.Second

// Agent drives one cluster/node's reconcile and status-report loops against an Executor.
type Agent struct {
	ClusterName string
	APIURL      string
	// Token is this cluster's runtime token, sent on every call to the control plane.
	Token string
	// Version is what the agent runs, its image tag, sent on every call beside the token
	// (clusterapi.AgentVersionHeader). Empty sends none.
	Version           string
	Executor          agentexec.Executor
	HTTPClient        *http.Client
	ReconcileInterval time.Duration
	StatusInterval    time.Duration
	// LogTailLines caps how many trailing lines FetchLogTail is asked for per status push.
	// Configurable per agent binary; DefaultLogTailLines if unset (zero).
	LogTailLines int
	// AutoscalerEnabled is the operator's own claim that this cluster sits behind a native
	// autoscaler (cluster-autoscaler / Karpenter) reacting to Pending pods — AUTOSCALER_ENABLED
	// env var, default false (fail-closed: a false positive costs a job a wasted scale-up
	// deadline, so this is never heuristically detected). See autoscaler.md.
	AutoscalerEnabled bool
	// Builder builds the environments the control plane assigns this cluster, which reports
	// that it builds. Nil builds none.
	Builder Builder
	Log     func(format string, args ...any)

	// unrunnable is what the last reconcile pass did not create, by experiment, with the attempt,
	// the reason and the runtime's words. It is that pass's observation and nothing more: every
	// pass replaces it whole, and a status push reports it beside what exists.
	unrunnableMu sync.Mutex
	unrunnable   map[string]refusedCreate

	// removed is set once the control plane said this cluster is no longer registered. From
	// then on there is nobody to report to.
	removed atomic.Bool

	// contacted is set by the first reconcile exchange the control plane answers, which is logged.
	contacted atomic.Bool

	// builds is what runs of the builds the last reconcile answer listed, by key: started when
	// listed, cancelled when no longer listed at that attempt, and kept once reported until then
	// so a finished build is not run again.
	buildsMu sync.Mutex
	builds   map[string]*runningBuild
	building sync.WaitGroup
}

// Builder builds one environment (envbuild.Daemon) and says how the build ended. credential is
// the registry login of the push, asked of the control plane each time the daemon needs it,
// gitCredential the read of the repository the file is of, asked each time a step mounts it,
// progress is told each step of the build as it starts and ends, and loaded the node its image is
// held on and its digest once it is there, before the push ends.
type Builder interface {
	Run(ctx context.Context, b clusterapi.EnvironmentBuild, credential func(ctx context.Context) (username, password string, err error),
		gitCredential func(ctx context.Context) (clusterapi.EnvironmentGitCredential, error), progress func(format string, args ...any), loaded func(node, digest string)) clusterapi.EnvironmentBuildReport
}

type runningBuild struct {
	attempt int
	cancel  context.CancelFunc
	// unsent is the outcome of a build that ended and whose report the control plane did not
	// take; posting is set while it is posted again.
	unsent  *clusterapi.EnvironmentBuildReport
	posting bool
}

// ErrClusterRemoved is what Run returns once the control plane no longer knows this cluster
// (its owner removed it, or the runtime detached it) and every workload it held is gone.
var ErrClusterRemoved = errors.New("this cluster was removed from the control plane")

type refusedCreate struct {
	attempt int
	reason  string
	why     string
}

// Run starts the reconcile and status-report loops and blocks until ctx is cancelled, which
// returns nil, or until the cluster was removed and its workloads are gone, which returns
// ErrClusterRemoved. ctx itself is left alone on that return: an executor's own operations run
// under it and finish what they started.
func (a *Agent) Run(ctx context.Context) error {
	statusCtx, stopStatus := context.WithCancel(ctx)
	defer stopStatus()
	var wg sync.WaitGroup
	wg.Add(2)
	var err error
	go func() { defer wg.Done(); a.statusReportLoop(statusCtx) }()
	go func() { defer wg.Done(); defer stopStatus(); err = a.reconcileLoop(ctx) }()
	wg.Wait()
	a.reconcileBuilds(ctx, nil)
	a.building.Wait()
	return err
}

// executorForPass is the executor one reconcile pass or one status push runs against: the
// agent's own, or the one a PassScoped executor hands out for the pass.
func (a *Agent) executorForPass(ctx context.Context) (agentexec.Executor, error) {
	scoped, ok := a.Executor.(agentexec.PassScoped)
	if !ok {
		return a.Executor, nil
	}
	return scoped.ForPass(ctx)
}

func (a *Agent) reconcileLoop(ctx context.Context) error {
	ticker := time.NewTicker(a.ReconcileInterval)
	defer ticker.Stop()
	for {
		err := a.reconcileOnce(ctx)
		if errors.Is(err, ErrClusterRemoved) {
			return err
		}
		if err != nil {
			a.Log("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *Agent) reconcileOnce(ctx context.Context) error {
	exec, err := a.executorForPass(ctx)
	if err != nil {
		return fmt.Errorf("read actual state: %w", err)
	}
	answer, err := a.fetchDesiredState(ctx, exec)
	// A removed cluster desires nothing: its jobs went back to the queue with the removal, so
	// the pass runs on as one whose every workload is no longer desired, until none is left.
	removed := errors.Is(err, ErrClusterRemoved)
	if err != nil && !removed {
		return fmt.Errorf("fetch desired state: %w", err)
	}
	if removed {
		a.removed.Store(true)
	}
	if !removed && !a.contacted.Swap(true) {
		a.Log("first contact with the control plane ok: %d workloads desired here", len(answer.Jobs))
	}
	a.reconcileBuilds(ctx, answer.Builds)
	desired := answer.Jobs
	checkpointWindow := make(map[string]bool, len(answer.CheckpointWindow))
	for _, id := range answer.CheckpointWindow {
		checkpointWindow[id] = true
	}
	actual, err := exec.ListManagedJobs(ctx)
	if err != nil {
		return fmt.Errorf("list local workloads: %w", err)
	}
	auxiliary, err := exec.ListManagedAuxiliaryWorkloads(ctx)
	if err != nil {
		return fmt.Errorf("list local auxiliary resources: %w", err)
	}
	if removed {
		// The status set also holds what is stopping and what stopped and is not reaped yet.
		held, err := exec.ListManagedJobsForStatus(ctx)
		if err != nil {
			return fmt.Errorf("list local workloads: %w", err)
		}
		if len(held) == 0 && len(auxiliary) == 0 {
			return ErrClusterRemoved
		}
		a.Log("this cluster was removed from the control plane: stopping the %d workloads it holds", len(held))
	}

	desiredByID := make(map[string]*clusterapi.DesiredJob, len(desired))
	for _, exp := range desired {
		if exp == nil || exp.ID == "" {
			return fmt.Errorf("desired state contains experiment without identity")
		}
		if _, exists := desiredByID[exp.ID]; exists {
			return fmt.Errorf("desired state contains duplicate experiment %q", exp.ID)
		}
		desiredByID[exp.ID] = exp
	}
	actualSet := make(map[string]bool, len(actual))
	for _, id := range actual {
		if id == "" {
			return fmt.Errorf("actual state contains workload without experiment identity")
		}
		if actualSet[id] {
			return fmt.Errorf("actual state contains duplicate experiment identity %q", id)
		}
		actualSet[id] = true
	}
	var reconcileErrors []error
	refused := map[string]refusedCreate{}

	for id, exp := range desiredByID {
		if actualSet[id] {
			matches, err := exec.WorkloadMatchesDesired(ctx, exp)
			if err != nil {
				a.Log("compare workload %s with desired spec: %v", id, err)
				reconcileErrors = append(reconcileErrors, fmt.Errorf("compare workload %s: %w", id, err))
				continue
			}
			if !matches {
				// No checkpoint window: this is not a termination at all. The workload is
				// being replaced with the one desired state now asks for, and the next pass
				// recreates it — making the job save a checkpoint it is about to be restarted
				// past would only delay its own replacement.
				if err := exec.DeleteWorkload(ctx, id, false); err != nil {
					a.Log("delete drifted workload %s: %v", id, err)
					reconcileErrors = append(reconcileErrors, fmt.Errorf("delete drifted workload %s: %w", id, err))
					continue
				}
				a.Log("deleted drifted workload %s; next pass recreates desired spec", id)
				continue
			}
			if refresher, ok := exec.(agentexec.PullRefresher); ok && exp.EnvironmentKey != "" {
				if err := refresher.RefreshPull(ctx, exp, answer.RegistryPull); err != nil {
					a.Log("refresh the pull login of %s: %v", id, err)
					reconcileErrors = append(reconcileErrors, fmt.Errorf("refresh the pull login of %s: %w", id, err))
				}
			}
			continue
		}
		if err := exec.CreateWorkload(ctx, exp, answer.RegistryPull); err != nil {
			// A job that waits is reported with the next status push and asked for again. Its
			// wait is logged when it begins or its reason changes, not every pass.
			var waiting *agentexec.Waiting
			if errors.As(err, &waiting) {
				refused[id] = refusedCreate{attempt: exp.AttemptCount, reason: waiting.Reason, why: waiting.Why}
				if a.lastRefused(id) != refused[id] {
					a.Log("workload %s waits: %s: %s", id, waiting.Reason, waiting.Why)
				}
				continue
			}
			// A job that can never be created here is the control plane's to end. Any other
			// failure is tried again next pass, and said as create_failed so its owner reads why
			// it waits. Both are told with the next status push, logged when they change, and
			// fail no pass.
			refused[id] = refusedCreate{attempt: exp.AttemptCount, reason: clusterapi.PhaseReasonCreateFailed, why: err.Error()}
			var unrunnable *agentexec.Unrunnable
			if errors.As(err, &unrunnable) {
				refused[id] = refusedCreate{attempt: exp.AttemptCount, reason: cmp.Or(unrunnable.Reason, clusterapi.PhaseReasonConfigError), why: unrunnable.Why}
			}
			if a.lastRefused(id) != refused[id] {
				a.Log("create workload %s: %v", id, err)
			}
			continue
		}
	}
	a.unrunnableMu.Lock()
	a.unrunnable = refused
	a.unrunnableMu.Unlock()

	// Deleting a workload destroys the only copy of why it ended: once the pod is gone, its log
	// tail and its container's termination reason are unreadable forever. Observe everything once
	// more and push before deleting, or a job that died between two status pushes reaches the
	// control plane as FAILED with nothing attached — which is what turned single crashes into
	// long blind debugging sessions.
	//
	// Deliberately the ordinary full status push, not a targeted one for the workloads about to
	// go: a push IS a complete cluster snapshot (metricsdb.RecordJobStatuses rejects anything
	// else), and a job missing from the newest snapshot reads as JobPhaseGone — which the
	// controller evicts. Pushing a snapshot containing only the dying jobs would therefore tell
	// the control plane that every other job on this cluster had vanished.
	//
	// Bounded, because the deletions must happen this pass whatever the control plane is doing:
	// when the budget is spent the workloads are simply deleted uncaptured, since a workload left
	// running is worse than a lost log line.
	needsFinalCapture := false
	for id := range actualSet {
		if desiredByID[id] == nil {
			needsFinalCapture = true
			break
		}
	}
	if needsFinalCapture && !removed {
		captureCtx, cancelCapture := context.WithTimeout(ctx, finalStatusCaptureBudget)
		a.reportStatuses(captureCtx, exec)
		cancelCapture()
	}

	for id := range actualSet {
		if desiredByID[id] != nil {
			continue
		}
		// The one place a checkpoint window can apply: this workload is no longer desired, so
		// something ended it, and the control plane has already said which endings earn a
		// window. A workload absent from the grant list is deleted as it always has been.
		if err := exec.DeleteWorkload(ctx, id, checkpointWindow[id]); err != nil {
			a.Log("delete workload %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete workload %s: %w", id, err))
			continue
		}
		a.Log("deleted workload %s (no longer desired)", id)
	}
	for _, id := range orphanAuxiliaryIDs(desiredByID, actualSet, auxiliary) {
		if err := exec.DeleteWorkload(ctx, id, false); err != nil {
			a.Log("delete orphan auxiliary resources %s: %v", id, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("delete orphan auxiliary resources %s: %w", id, err))
			continue
		}
		a.Log("deleted orphan auxiliary resources %s", id)
	}

	if err := exec.ReapTerminal(ctx, desiredByID); err != nil {
		a.Log("reap terminal workloads: %v", err)
		reconcileErrors = append(reconcileErrors, fmt.Errorf("reap terminal workloads: %w", err))
	}
	return errors.Join(reconcileErrors...)
}

func orphanAuxiliaryIDs(desired map[string]*clusterapi.DesiredJob, actualJobs map[string]bool, auxiliary []string) []string {
	orphans := make([]string, 0, len(auxiliary))
	for _, id := range auxiliary {
		if desired[id] == nil && !actualJobs[id] {
			orphans = append(orphans, id)
		}
	}
	return orphans
}

// fetchDesiredState returns this cluster's desired state. A removed cluster's is empty.
func (a *Agent) fetchDesiredState(ctx context.Context, exec agentexec.Executor) (clusterapi.ReconcileAnswer, error) {
	cpuAvail, cpuTotal, err := exec.GetLiveCPUCapacity(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get live CPU capacity: %w", err)
	}
	acceleratorAvail, acceleratorTotal, acceleratorByNode, nodeLabels, err := exec.GetLiveAcceleratorCapacitySnapshot(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get live accelerator capacity: %w", err)
	}
	ramAvail, ramTotal, err := exec.GetLiveRAMCapacity(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get live RAM capacity: %w", err)
	}
	storageAvail, storageTotal, err := exec.GetLiveStorageCapacity(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get live storage capacity: %w", err)
	}
	nodeResources, err := exec.GetLiveNodeResourceCapacity(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get live per-node resource capacity: %w", err)
	}
	nodeResourcesTotal, err := exec.GetNodeTotalCapacity(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get per-node total resource capacity: %w", err)
	}
	clusterID, err := exec.GetClusterID(ctx)
	if err != nil {
		return clusterapi.ReconcileAnswer{}, fmt.Errorf("get cluster id: %w", err)
	}
	report := clusterapi.ReconcileRequest{
		CPUAvailableCores: cpuAvail, CPUTotalCores: cpuTotal,
		AcceleratorAvailableByFlavor: acceleratorAvail, AcceleratorTotalByFlavor: acceleratorTotal,
		AcceleratorAvailableByNode: acceleratorByNode,
		NodeResourcesByNode:        nodeResources,
		NodeResourcesTotalByNode:   nodeResourcesTotal,
		NodeLabelsByNode:           nodeLabels,
		MultiNodeCapable:           exec.SupportsMultiNodeJobs(),
		JobImage:                   exec.OnlyImage(),
		AutoscalerEnabled:          a.AutoscalerEnabled,
		Builds:                     a.Builder != nil,
		ClusterID:                  clusterID,
		RAMAvailableBytes:          ramAvail, RAMTotalBytes: ramTotal,
		StorageAvailableBytes: storageAvail, StorageTotalBytes: storageTotal,
	}
	var answer clusterapi.ReconcileAnswer
	if err := a.call(ctx, http.MethodPost, "/reconcile", report, &answer); err != nil {
		return clusterapi.ReconcileAnswer{}, err
	}
	return answer, nil
}

func (a *Agent) statusReportLoop(ctx context.Context) {
	ticker := time.NewTicker(a.StatusInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.removed.Load() {
				a.reportChangedStatuses(ctx)
			}
		}
	}
}

func (a *Agent) reportChangedStatuses(ctx context.Context) {
	exec, err := a.executorForPass(ctx)
	if err != nil {
		a.Log("read actual state for status: %v", err)
		return
	}
	a.reportStatuses(ctx, exec)
}

// reportStatuses pushes one snapshot observed through exec. The reconcile pass hands it its own
// executor for the last push before a delete, so that push costs no second read of actual state.
func (a *Agent) reportStatuses(ctx context.Context, exec agentexec.Executor) {
	ids, err := exec.ListManagedJobsForStatus(ctx)
	if err != nil {
		a.Log("list managed jobs for status: %v", err)
		return
	}

	// All or nothing, deliberately. A push is a complete cluster snapshot — the control plane
	// rejects anything else (metricsdb.RecordJobStatuses) and reads a job missing from the newest
	// snapshot as JobPhaseGone, which the controller evicts. So omitting a job we merely failed to
	// observe would report it as vanished and get it killed, which is far worse than this tick
	// reporting nothing: a cluster with no fresh snapshot reads as "cannot tell", and the
	// controller declines to act on it (see checkSilence's !found branch).
	reports := make([]clusterapi.StatusReport, 0, len(ids))
	for _, id := range ids {
		report, ok := a.statusReportFor(ctx, exec, id)
		if !ok {
			a.Log("status snapshot incomplete (%s unobservable); skipping this push rather than reporting the rest as gone", id)
			return
		}
		reports = append(reports, report)
	}
	reports = append(reports, a.refusedReports(ids)...)

	a.pushStatus(ctx, reports)
}

// refusedReports is the status of every job the last reconcile pass did not create and that has
// no workload: pending, under the reason the runtime gave, which the control plane ends the job
// for when it is one that never heals, and with the runtime's words as the message its owner
// reads.
func (a *Agent) refusedReports(existing []string) []clusterapi.StatusReport {
	a.unrunnableMu.Lock()
	defer a.unrunnableMu.Unlock()
	var reports []clusterapi.StatusReport
	for id, refused := range a.unrunnable {
		if slices.Contains(existing, id) {
			continue
		}
		attempt := refused.attempt
		reports = append(reports, clusterapi.StatusReport{
			ExperimentID: id, Attempt: &attempt, Phase: clusterapi.JobPhasePending.String(),
			Reason: refused.reason, Message: refused.why,
		})
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].ExperimentID < reports[j].ExperimentID })
	return reports
}

// lastRefused is what the pass before this one said of id.
func (a *Agent) lastRefused(id string) refusedCreate {
	a.unrunnableMu.Lock()
	defer a.unrunnableMu.Unlock()
	return a.unrunnable[id]
}

// statusReportFor observes one job: its phase, where it landed, its log tail and why its
// container is in the state it is in. ok is false when the job could not be observed coherently,
// which costs the whole snapshot — see reportChangedStatuses for why a partial one is dangerous.
func (a *Agent) statusReportFor(ctx context.Context, exec agentexec.Executor, id string) (clusterapi.StatusReport, bool) {
	// One combined poll keeps phase, UID and attempt from separate reads from being combined
	// into an impossible observation during a delete/recreate race.
	observed, err := exec.PollJobStatus(ctx, id)
	if err != nil {
		a.Log("poll job phase %s: %v", id, err)
		return clusterapi.StatusReport{}, false
	}
	phase := observed.Phase
	// Attribution is reported when it can be resolved and left empty when it cannot. It says
	// which accelerator the job landed on, not whether the job exists, so failing to read it is
	// not grounds to withhold the phase — and withholding the phase costs the whole cluster's
	// snapshot. A job between pods during a delete-and-recreate has a phase but no attribution
	// yet, which used to blank out status for every other job in the cluster until it settled.
	// The control plane bounds an unattributable job on its own (see job_watcher's
	// runningTypeUnobservable), which it can only do if it keeps hearing the phase.
	var admittedAcceleratorType, admittedNode string
	var admittedMachine *clusterapi.JobMachine
	if phase != clusterapi.JobPhaseGone {
		t, node, consistent, err := exec.ResolveAdmittedAcceleratorType(ctx, id)
		switch {
		case err != nil:
			a.Log("resolve admitted accelerator type %s: %v", id, err)
		case !consistent:
			a.Log("experiment %s: scheduled ranks landed on inconsistent accelerator types", id)
		default:
			admittedAcceleratorType = string(t)
			admittedNode = node
			if node != "" {
				admittedMachine = observed.Machine
			}
		}
	}

	// A job the runtime no longer holds has nothing left to read: skipped because there is no
	// source, not because reading it failed.
	var logTail []string
	var reason, message string
	var restartCount, scheduledNodes int32
	var schedulingReason string
	if phase != clusterapi.JobPhaseGone {
		// Diagnostics accompany the phase; they are not the phase. A read error on one job must
		// not stop this batch reporting phase for every other job (important.md #19), so it is
		// logged and that job reports phase alone.
		logTail, err = exec.FetchLogTail(ctx, id, a.logTailLines())
		if err != nil {
			a.Log("fetch log tail %s: %v", id, err)
			logTail = nil
		}
		reason, message, restartCount, scheduledNodes, schedulingReason, err = exec.PollPhaseDetail(ctx, id, observed.Attempt)
		if err != nil {
			a.Log("poll phase detail %s: %v", id, err)
			reason, message, restartCount, scheduledNodes, schedulingReason = "", "", 0, 0, ""
		}
	}

	var attempt *int
	if observed.Attempt != clusterapi.AttemptUnknown {
		attempt = &observed.Attempt
	}

	var startedAt, endedAt *time.Time
	if !observed.EndedAt.IsZero() {
		startedAt, endedAt = &observed.StartedAt, &observed.EndedAt
	}

	return clusterapi.StatusReport{
		ExperimentID:            id,
		Attempt:                 attempt,
		StartedAt:               startedAt,
		EndedAt:                 endedAt,
		Phase:                   phase.String(),
		AdmittedAcceleratorType: admittedAcceleratorType,
		AdmittedNode:            admittedNode,
		AdmittedMachine:         admittedMachine,
		LogTail:                 logTail,
		Reason:                  reason,
		Message:                 message,
		RestartCount:            restartCount,
		ScheduledNodes:          scheduledNodes,
		SchedulingReason:        schedulingReason,
	}, true
}

// controlPlane is this cluster's API on the control plane.
func (a *Agent) controlPlane() *cpclient.Client {
	client := &cpclient.Client{BaseURL: fmt.Sprintf("%s/internal/clusters/%s", a.APIURL, a.ClusterName), Token: a.Token, HTTP: a.HTTPClient}
	if a.Version != "" {
		client.Header = http.Header{clusterapi.AgentVersionHeader: {a.Version}}
	}
	return client
}

// call makes one request of this cluster's API. A 401 that says the cluster is no longer
// registered is ErrClusterRemoved; any other refusal is the control plane's own words, and the
// loop goes on either way: a 401 or 403 is a token or a registration somebody has to fix, and the
// reason is what tells them which.
func (a *Agent) call(ctx context.Context, method, path string, body, out any) error {
	err := a.controlPlane().Do(ctx, method, path, body, out)
	var refused *cpclient.Refusal
	if errors.As(err, &refused) && refused.Status == http.StatusUnauthorized && refused.Reason == clusterapi.ReasonClusterNotRegistered {
		return ErrClusterRemoved
	}
	return err
}

// Detach ends this cluster on the control plane, as its owner would by removing it: its jobs go
// back to the queue, named in requeued, and its runtime token is refused from then on. The
// running agent reads that refusal, stops the workloads it holds and exits.
func (a *Agent) Detach(ctx context.Context) (requeued []string, err error) {
	var answer clusterapi.DetachAnswer
	if err := a.call(ctx, http.MethodDelete, "", nil, &answer); err != nil {
		return nil, err
	}
	return answer.Requeued, nil
}

func (a *Agent) pushStatus(ctx context.Context, reports []clusterapi.StatusReport) {
	var ack clusterapi.StatusAck
	if err := a.call(ctx, http.MethodPost, "/status", clusterapi.StatusPush{Reports: reports}, &ack); err != nil {
		a.Log("push status: %v", err)
		return
	}
	if ack.Status != "ok" {
		a.Log("push status: invalid acknowledgement status %q", ack.Status)
	}
}

// RequiredEnv reads an environment variable or exits the process with a labeled error — the
// startup-validation shape shared by every agent binary's main().
func RequiredEnv(binaryName, key string) string {
	value := os.Getenv(key)
	if value == "" {
		fmt.Fprintf(os.Stderr, "%s: %s environment variable is required\n", binaryName, key)
		os.Exit(1)
	}
	return value
}

// OptionalIntEnv reads an integer environment variable, returning 0 when it is unset and exiting
// the process with a labeled error when it is set but malformed.
func OptionalIntEnv(binaryName, key string) int {
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s=%q is not an integer\n", binaryName, key, raw)
		os.Exit(1)
	}
	return value
}

// OptionalBoolEnv reads a boolean environment variable, returning false when it is unset and
// exiting the process with a labeled error when it is set but malformed.
func OptionalBoolEnv(binaryName, key string) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s=%q is not a boolean\n", binaryName, key, raw)
		os.Exit(1)
	}
	return value
}

// SignalContext returns a context cancelled on SIGTERM/SIGINT, logging via logFn when that
// happens — the shutdown-handling shape shared by every agent binary's main().
func SignalContext(logFn func(format string, args ...any)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		logFn("shutting down")
		cancel()
	}()
	return ctx, cancel
}

// logTailLines is how many trailing log lines a status report carries.
func (a *Agent) logTailLines() int {
	if a.LogTailLines <= 0 {
		return DefaultLogTailLines
	}
	return a.LogTailLines
}

// CheckToken asks the control plane whether it takes this cluster's token, and changes nothing
// there: no capacity is reported and no identity bound. An installer's dry run asks it.
func (a *Agent) CheckToken(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, "/access", nil, nil)
}

// CheckReachable asks base's /healthz, which answers without a token.
func CheckReachable(ctx context.Context, client *http.Client, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/healthz answered %d", base, resp.StatusCode)
	}
	return nil
}

// CheckAccess makes the call every pass starts with and acts on nothing it answers, so an
// installer learns that the control plane refuses this cluster's token before it starts an
// agent that would only log the refusal.
func (a *Agent) CheckAccess(ctx context.Context) error {
	_, err := a.fetchDesiredState(ctx, a.Executor)
	return err
}

// RunDetach is the detach command of an agent binary: it ends the cluster named by the
// environment the agent itself runs with and says what became of its jobs. A cluster already
// removed is the state asked for.
func RunDetach(ctx context.Context, binaryName string, log func(format string, args ...any)) *Agent {
	a := &Agent{
		ClusterName: RequiredEnv(binaryName, "CLUSTER_NAME"),
		APIURL:      RequiredEnv(binaryName, "API_URL"),
		Token:       RequiredEnv(binaryName, "RUNTIME_TOKEN"),
		HTTPClient:  &http.Client{Timeout: 35 * time.Second},
		Log:         log,
	}
	requeued, err := a.Detach(ctx)
	switch {
	case errors.Is(err, ErrClusterRemoved):
		log("cluster %s is already removed from %s", a.ClusterName, a.APIURL)
	case err != nil:
		fmt.Fprintf(os.Stderr, "%s: detach %s: %v\n", binaryName, a.ClusterName, err)
		os.Exit(1)
	default:
		log("detached cluster %s from %s: %d of its jobs went back to the queue", a.ClusterName, a.APIURL, len(requeued))
	}
	return a
}
