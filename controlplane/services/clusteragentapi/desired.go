package clusteragentapi

import (
	"errors"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// desiredJob is exp as a cluster's runtime is handed it: what a runtime builds a workload from,
// and nothing it has no use for. A runtime may be another org's, so the hypothesis, the
// objective, the theory and where the job sits in its org stay with the platform. This is the
// one place the job's secret_env leaves the platform: on the spec, where the runtime reads the
// rest of the job's environment.
//
// The job is handed its durable-data address, derived from its own identity, with the permanent
// key stored with it; the store already limits that key to writing the job's own prefix and
// reading its platform experiment's, and nothing here calls the store. It is handed the token it
// reports metrics and fetches its commit with, derived from its id, so every reconcile answers
// with the same one, and its platform experiment's repository through the git proxy.
func (h *Handler) desiredJob(exp *domain.Experiment) (*clusterapi.DesiredJob, error) {
	token, err := auth.Mint(h.signingSecret, auth.JobScope(exp.ID))
	if err != nil {
		return nil, err
	}
	if h.proxyURL == "" {
		return nil, errors.New("no git proxy address to hand jobs their repository at")
	}
	job := &clusterapi.DesiredJob{
		ID:                     exp.ID,
		AgentID:                exp.AgentID,
		ProjectID:              exp.ProjectID,
		CodeRef:                exp.CodeRef,
		ConfigHash:             exp.ConfigHash,
		DataRef:                exp.DataRef,
		Job:                    jobSpec(exp.Job, exp.SecretEnv),
		EnvironmentKey:         exp.EnvironmentKey,
		AcceleratorType:        clusterapi.AcceleratorType(exp.AcceleratorType),
		AcceleratorCount:       exp.AcceleratorCount,
		EstimatedDurationHours: exp.EstimatedDurationHours,
		CapacityTier:           clusterapi.CapacityTier(exp.CapacityTier),
		SubmittedAt:            exp.SubmittedAt,
		Data: &clusterapi.DataAccess{
			URI:             h.dataStore.URI(objectstore.JobPrefix(exp.PlatformExperimentID, exp.AgentID, exp.ID)),
			Shared:          h.dataStore.URI(objectstore.PlatformExperimentPrefix(exp.PlatformExperimentID)),
			Endpoint:        h.dataStore.Endpoint,
			Region:          h.dataStore.Region,
			AccessKeyID:     exp.DataKey.AccessKeyID,
			SecretAccessKey: exp.DataKey.SecretAccessKey,
		},
		Token:        token,
		GitURL:       domain.GitProxyURL(h.proxyURL, exp.PlatformExperimentID),
		AttemptCount: exp.AttemptCount,
	}
	if exp.ResolvedJob != nil {
		resolved := jobSpec(*exp.ResolvedJob, exp.SecretEnv)
		job.ResolvedJob = &resolved
	}
	return job, nil
}

func jobSpec(j domain.JobSpec, secretEnv map[string]string) clusterapi.JobSpec {
	spec := clusterapi.JobSpec{
		Image:                         j.Image,
		Command:                       j.Command,
		Args:                          j.Args,
		Env:                           j.Env,
		SecretEnv:                     secretEnv,
		CPU:                           j.CPU,
		Memory:                        j.Memory,
		Storage:                       j.Storage,
		AcceleratorCount:              j.AcceleratorCount,
		AcceleratorType:               clusterapi.AcceleratorType(j.AcceleratorType),
		NodeSelector:                  j.NodeSelector,
		AcceleratorTolerations:        j.AcceleratorTolerations,
		AcceleratorPodResources:       j.AcceleratorPodResources,
		NumNodes:                      j.NumNodes,
		MaxRetries:                    j.MaxRetries,
		TerminationGracePeriodSeconds: j.TerminationGracePeriodSeconds,
		CheckpointGraceSeconds:        j.CheckpointGraceSeconds,
		ShmSize:                       j.ShmSize,
		ExtraResources:                j.ExtraResources,
		HostMounts:                    j.HostMounts,
	}
	for _, t := range j.AcceptableAcceleratorTypes {
		spec.AcceptableAcceleratorTypes = append(spec.AcceptableAcceleratorTypes, clusterapi.AcceleratorType(t))
	}
	for _, g := range j.Groups {
		spec.Groups = append(spec.Groups, clusterapi.JobGroup{
			Name: g.Name, Replicas: g.Replicas, CPU: g.CPU, Memory: g.Memory, Storage: g.Storage,
			AcceleratorCount: g.AcceleratorCount, AcceleratorType: clusterapi.AcceleratorType(g.AcceleratorType),
			Command: g.Command, Args: g.Args, Env: g.Env,
		})
	}
	if j.Topology != nil {
		spec.Topology = &clusterapi.TopologySpec{SpreadAcrossHosts: j.Topology.SpreadAcrossHosts, SameZone: j.Topology.SameZone}
	}
	return spec
}

// jobMachine is the machine a runtime reported a job on, as the platform keeps it.
func jobMachine(m *clusterapi.JobMachine) *domain.JobMachine {
	if m == nil {
		return nil
	}
	return &domain.JobMachine{InstanceID: m.InstanceID, HostID: m.HostID, AcceleratorModel: m.AcceleratorModel, CPUModel: m.CPUModel, PricePerHourUSD: m.PricePerHourUSD}
}
