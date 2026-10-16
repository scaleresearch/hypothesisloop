package scheduler

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// environmentPass is what one tick read of the environments queued jobs wait for: each
// environment once, after its build was assigned, and the live clusters once, when a first
// environment is assigned.
type environmentPass struct {
	seen    map[string]*domain.Environment
	live    []string
	holding []string
	read    bool
}

func newEnvironmentPass() *environmentPass {
	return &environmentPass{seen: map[string]*domain.Environment{}}
}

// assignBuild assigns the build of the environment of key to one of the live clusters the pass
// read, reading them first if it has not (db.EnvironmentsStore.AssignEnvironment).
func (l *Loop) assignBuild(ctx context.Context, pass *environmentPass, key string) (*domain.Environment, error) {
	if !pass.read {
		live, err := l.clusters.GetLiveClusters(ctx)
		if err != nil {
			return nil, fmt.Errorf("live clusters: %w", err)
		}
		for name, connected := range live {
			if connected {
				pass.live = append(pass.live, name)
			}
			pass.holding = append(pass.holding, name)
		}
		pass.read = true
	}
	return l.store.AssignEnvironment(ctx, key, pass.live, pass.holding, domain.EnvironmentBuildLease)
}

// AssignBuild assigns the build of the environment of key to a live cluster now, as a tick does
// for a job waiting for it, and returns the environment as it is then, nil when there is none.
// It is one statement, so it races with no tick.
func (l *Loop) AssignBuild(ctx context.Context, key string) (*domain.Environment, error) {
	return l.assignBuild(ctx, newEnvironmentPass(), key)
}

// heldForEnvironment says whether exp waits for the environment its commit declares, whose build
// it assigns to a live cluster first (db.EnvironmentsStore.AssignEnvironment): while it is built
// it says so, once the file failed exp ends with the end of the build's output, so its agent fixes
// the file and submits again, and once the platform failed it on every attempt exp ends as the
// platform's fault at no cost, and the next submit builds it again. A build its cluster already
// holds on one node does not hold exp: exp is pinned there for this pass (pinToLoadedNode).
func (l *Loop) heldForEnvironment(ctx context.Context, pass *environmentPass, exp *domain.Experiment, registered map[string]domain.Cluster, nodeLabels map[string]map[string]map[string]string) (bool, error) {
	env, ok := pass.seen[exp.EnvironmentKey]
	if !ok {
		var err error
		if env, err = l.assignBuild(ctx, pass, exp.EnvironmentKey); err != nil {
			return false, err
		}
		if env == nil {
			return false, fmt.Errorf("job %s names environment %s, which does not exist", exp.ID, exp.EnvironmentKey)
		}
		pass.seen[exp.EnvironmentKey] = env
	}
	switch {
	case env.State == domain.EnvironmentReady:
		exp.EnvironmentCluster = env.ClusterName
		return false, nil
	case env.State == domain.EnvironmentBuilding:
		if pinToLoadedNode(exp, env, registered, nodeLabels) {
			return false, nil
		}
		reason := domain.NotAdmittedEnvironmentBuilding + ": " + buildingMessage(env, false)
		return true, l.markNotAdmitted(ctx, exp, reason)
	case env.State == domain.EnvironmentFailed && env.Fault == domain.EnvironmentFaultFile:
		return true, l.endForEnvironment(ctx, exp, domain.StatusRejected, string(domain.EvictionEnvironmentFailed.WithDetail(fileFailed(env))))
	case env.State == domain.EnvironmentFailed && env.Fault == domain.EnvironmentFaultPlatform:
		return true, l.endForEnvironment(ctx, exp, domain.StatusEvicted, string(domain.EvictionEnvironmentUnbuilt.WithDetail("the platform could not build the environment its commit declares in "+domain.EnvironmentFile+
			" on "+env.Base+" in "+strconv.Itoa(domain.MaxEnvironmentAttempts)+" attempts, no fault of the file's. It costs nothing: submit again and the platform builds it again")))
	}
	return false, fmt.Errorf("environment %s of job %s is %s with fault %q", env.Key, exp.ID, env.State, env.Fault)
}

// pinToLoadedNode pins exp, for this pass, to the node of the cluster that built its environment
// and holds the build's image before it is pushed, and to that image (domain.Experiment.
// EnvironmentImage), and says whether it did. The pin is the node's hostname label in exp's node
// selector, which placement honours as any other and the runtime is handed with the resolved job:
// a job placed so starts there without a pull while the push goes on. A job on more than one node,
// one whose own selector names another node, and one that cluster does not host wait for the push.
func pinToLoadedNode(exp *domain.Experiment, env *domain.Environment, registered map[string]domain.Cluster, nodeLabels map[string]map[string]map[string]string) bool {
	if env.LoadedImage == "" || env.LoadedCluster == "" || exp.Job.Nodes() > 1 || !hostsJob(registered, env.LoadedCluster, exp) {
		return false
	}
	hostname := nodeLabels[env.LoadedCluster][env.LoadedNode][domain.NodeHostnameLabel]
	if hostname == "" {
		return false
	}
	if own, ok := exp.Job.NodeSelector[domain.NodeHostnameLabel]; ok && own != hostname {
		return false
	}
	selector := make(map[string]string, len(exp.Job.NodeSelector)+1)
	maps.Copy(selector, exp.Job.NodeSelector)
	selector[domain.NodeHostnameLabel] = hostname
	exp.Job.NodeSelector = selector
	exp.ClusterName = env.LoadedCluster
	exp.EnvironmentImage = env.LoadedImage
	return true
}

// waitForPush leaves exp, pinned to the node its environment is loaded on, queued for this pass:
// that node has no room for it now, and nothing is preempted or bet for a wait that ends with the
// push, after which it is placed anywhere.
func (l *Loop) waitForPush(ctx context.Context, pass string, environments *environmentPass, exp *domain.Experiment, tickErrs *[]error) {
	obsmetrics.AdmissionTickResultsTotal.WithLabelValues(pass, "skipped").Inc()
	reason := domain.NotAdmittedEnvironmentBuilding + ": " + buildingMessage(environments.seen[exp.EnvironmentKey], false)
	if err := l.markNotAdmitted(ctx, exp, reason); err != nil {
		l.skipExperiment(tickErrs, exp, fmt.Errorf("mark not admitted: %w", err))
	}
}

// buildingMessage is what a job waiting for env to build is told. Where the build runs, its attempt
// and its last step are said only with live, on a read: the stored queue reason changes with none
// of them, since its every change is an update that wakes the job's agent.
func buildingMessage(env *domain.Environment, live bool) string {
	message := "the platform builds the environment its commit declares in " + domain.EnvironmentFile + ", on " + env.Base + ". The job runs in it once it is built"
	if live {
		message += ". " + buildProgress(env)
	}
	if env.FailedLog != "" {
		message += ". This file failed to build before, and the platform builds it once more. The end of that build's output:\n" + env.FailedLog
	}
	return message
}

func buildProgress(env *domain.Environment) string {
	var progress string
	switch {
	case env.LoadedImage != "" && env.LoadedCluster == env.ClusterName:
		progress = "It is built and loaded on node " + env.LoadedNode + " of cluster " + env.LoadedCluster + ", which pushes it to the registry now: " +
			"a job of one node that fits there runs there at once, any other once the push ends. "
	case env.LoadedImage != "":
		progress = "It is built and loaded on node " + env.LoadedNode + " of cluster " + env.LoadedCluster + ", whose push to the registry failed: " +
			"a job of one node that fits there runs there at once, any other once the platform has built and pushed it again. "
	}
	if env.ClusterName == "" {
		return progress + "It waits for a live cluster that builds for its org to have room"
	}
	progress += "It builds on cluster " + env.ClusterName + ", attempt " + strconv.Itoa(env.Attempt) + " of " + strconv.Itoa(domain.MaxEnvironmentAttempts)
	if env.Attempt > 1 {
		progress += " (the platform builds a file again only after a failure of its own)"
	}
	if env.Progress != "" && env.ProgressAt != nil {
		progress += ", and reported at " + env.ProgressAt.UTC().Format(time.RFC3339) + ": " + env.Progress
	}
	return progress
}

// fileFailed says that the environment file of env did not build, with the end of its output.
func fileFailed(env *domain.Environment) string {
	return domain.EnvironmentFile + " of its commit did not build on " + env.Base + ". Fix it, commit and submit again; the same file submitted again is built once more. The end of the build's output:\n" + env.LogTail
}

// endForEnvironment ends exp, queued for its environment, as to with reason, settled: its usage
// is written first and the hold goes with the status, so the agent the status notifies reads its
// quota as it stands. An environment the platform did not build costs nothing.
func (l *Loop) endForEnvironment(ctx context.Context, exp *domain.Experiment, to domain.ExperimentStatus, reason string) error {
	ended := *exp
	ended.Status, ended.EvictionReason = to, reason
	if err := l.settler.Settle(ctx, &ended); err != nil {
		return fmt.Errorf("settle job %s whose environment did not build: %w", exp.ID, err)
	}
	written, err := l.store.EndQueuedSettled(ctx, exp.ID, to, reason)
	if err != nil || !written {
		return err
	}
	*exp = ended
	return nil
}
