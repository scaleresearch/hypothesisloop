package clusteragentapi

import (
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// wireCase is one representative desired state of a cluster, as the store holds it.
type wireCase struct {
	exps             []*domain.Experiment
	checkpointWindow []string
	builds           []domain.EnvironmentBuild
	registryPull     *domain.RegistryLogin
}

func wireHandler(t *testing.T) *Handler {
	t.Helper()
	store, err := objectstore.New("minio", "http://127.0.0.1:1", "http://minio.test:9000", "us-east-1", "hl-data", "root", "root-secret")
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{dataStore: store, signingSecret: "test-signing-secret-of-enough-length", proxyURL: "https://api.test/git"}
}

func wireCases() map[string]wireCase {
	submitted := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	shutdown, checkpoint, shortShutdown := int64(30), int64(120), int64(10)
	retries := 2
	spread := false
	return map[string]wireCase{
		"single_node_resolved": {
			exps: []*domain.Experiment{{
				ID: "job-1", AgentID: "agent-a", AgentName: "Agent A", PlatformExperimentID: "pe-1",
				ProjectID: "proj-1", OrgID: "org-1", ClusterName: "edge-7",
				CodeRef: "https://git.test/r.git@0123456789abcdef0123456789abcdef01234567", Branch: "agent-a/try",
				ConfigHash: "cfg-1", DataRef: "data-1", HypothesisID: "hyp-1", Hypothesis: "bigger lr", Theory: "converges faster",
				Job: domain.JobSpec{
					Image: "registry.test/job:1", Command: []string{"bash", "-c", "python train.py"}, Args: []string{"--lr", "0.1"},
					Env: map[string]string{"LR": "0.1"}, CPU: "8", Memory: "32Gi", Storage: "100Gi",
					AcceleratorCount: 1, AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
					ShmSize: "8Gi", TerminationGracePeriodSeconds: &shutdown, CheckpointGraceSeconds: &checkpoint,
				},
				ResolvedJob: &domain.JobSpec{
					Image: "registry.test/job:1", Command: []string{"bash", "-c", "python train.py"}, Args: []string{"--lr", "0.1"},
					Env: map[string]string{"LR": "0.1"}, CPU: "16", Memory: "64Gi", Storage: "200Gi",
					AcceleratorCount: 1, AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3",
					NodeSelector:            map[string]string{domain.NodeSlotLabel: "slot-3"},
					AcceleratorPodResources: map[string]string{"hugepages-1Gi": "4Gi"},
					ShmSize:                 "8Gi", TerminationGracePeriodSeconds: &shutdown, CheckpointGraceSeconds: &checkpoint,
				},
				EnvironmentKey:  "env-abc",
				AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", AcceleratorCount: 1,
				EstimatedDurationHours: 2.5, EstimatedCostAccH: 2.5, CapacityTier: domain.CapacityGuaranteed,
				PriorityScore: 0.7, Status: domain.StatusSubmitted, SubmittedAt: &submitted, QueuedAt: &created,
				AttemptCount: 3, InfraRequeueCount: 1, InfraRequeueReason: "stuck_pending",
				DataKey:   domain.DataKey{AccessKeyID: "AK1", SecretAccessKey: "SK1"},
				SecretEnv: map[string]string{"HF_TOKEN": "the-secret-value"},
				CreatedAt: created, UpdatedAt: submitted,
			}},
			checkpointWindow: []string{"job-0"},
			builds: []domain.EnvironmentBuild{{
				Key: "env-def", Attempt: 1, Base: "registry.test/job@sha256:aa", Tools: "registry.test/tools@sha256:bb",
				Dockerfile: "ARG BASE\nFROM ${BASE}\n", Repository: "registry.test/envs", CacheScope: "hl-0123",
			}},
			registryPull: &domain.RegistryLogin{Registry: "registry.test", Username: "puller", Password: "pw", ExpiresAt: submitted.Add(time.Hour)},
		},
		"grouped_multi_node": {
			exps: []*domain.Experiment{{
				ID: "job-2", AgentID: "agent-b", PlatformExperimentID: "pe-1", ProjectID: "proj-1", OrgID: "org-1",
				CodeRef: "https://git.test/r.git@89abcdef0123456789abcdef0123456789abcdef", ConfigHash: "cfg-2",
				Job: domain.JobSpec{
					Image: "registry.test/job:2", Env: map[string]string{"SHARED": "1"},
					AcceleratorType:            "tenstorrent.com/chipArch=blackhole",
					AcceptableAcceleratorTypes: []domain.AcceleratorType{"tenstorrent.com/chipArch=wormhole"},
					NodeSelector:               map[string]string{"zone": "a"},
					AcceleratorTolerations:     []string{"tenstorrent.com/chip"},
					Groups: []domain.JobGroup{
						{Name: "learner", Replicas: 1, CPU: "32", Memory: "128Gi", Storage: "500Gi", AcceleratorCount: 4,
							AcceleratorType: "tenstorrent.com/chipArch=blackhole", Command: []string{"python", "learn.py"}, Env: map[string]string{"ROLE": "learner"}},
						{Name: "actor", Replicas: 3, CPU: "4", Memory: "8Gi", Storage: "10Gi", Args: []string{"--actor"}},
					},
					MaxRetries:                    &retries,
					TerminationGracePeriodSeconds: &shortShutdown,
					Topology:                      &domain.TopologySpec{SpreadAcrossHosts: &spread, SameZone: true},
					ExtraResources:                map[string]string{"example.com/fpga": "1"},
					HostMounts:                    map[string]string{"/data": "/mnt/datasets/imagenet"},
				},
				AcceleratorType: "tenstorrent.com/chipArch=blackhole", AcceleratorCount: 4,
				EstimatedDurationHours: 1, CapacityTier: domain.CapacityBurst, Status: domain.StatusRunning,
				SubmittedAt: &submitted,
				DataKey:     domain.DataKey{AccessKeyID: "AK2", SecretAccessKey: "SK2"},
				CreatedAt:   created, UpdatedAt: submitted,
			}},
			builds: []domain.EnvironmentBuild{},
		},
		"cpu_job_unresolved_and_empty_lists": {
			exps: []*domain.Experiment{{
				ID: "job-3", AgentID: "agent-c", PlatformExperimentID: "pe-2", ProjectID: "proj-2",
				Job:             domain.JobSpec{Image: "registry.test/job:3", CPU: "2", Memory: "8Gi", Storage: "4Gi"},
				AcceleratorType: domain.CPUType, AcceleratorCount: 2, CapacityTier: domain.CapacityGuaranteed,
				DataKey: domain.DataKey{AccessKeyID: "AK3", SecretAccessKey: "SK3"},
			}},
			builds: []domain.EnvironmentBuild{},
		},
		"no_jobs": {
			exps:   []*domain.Experiment{},
			builds: []domain.EnvironmentBuild{},
		},
	}
}
