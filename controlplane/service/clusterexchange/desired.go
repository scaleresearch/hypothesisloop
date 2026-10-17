package clusterexchange

import (
	"context"
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/scope"
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
// with the same one, its platform experiment's repository through the git proxy, the launch
// script every node runs, and a login that pulls its image when the image needs one. A login that
// cannot be minted fails the whole answer, so a runtime acts on nothing rather than on a job it
// cannot pull.
func (s *Service) desiredJob(ctx context.Context, exp *domain.Experiment) (*clusterapi.DesiredJob, error) {
	token, err := auth.Mint(s.signingSecret, scope.Job(exp.ID))
	if err != nil {
		return nil, err
	}
	if s.proxyURL == "" {
		return nil, errors.New("no git proxy address to hand jobs their repository at")
	}
	if exp.Image == "" {
		return nil, fmt.Errorf("job %s has no image", exp.ID)
	}
	pull, err := s.pulls(ctx, exp.OrgID, exp.Image)
	if err != nil {
		return nil, fmt.Errorf("the pull login of job %s: %w", exp.ID, err)
	}
	job := &clusterapi.DesiredJob{
		ID:                     exp.ID,
		AgentID:                exp.AgentID,
		ProjectID:              exp.ProjectID,
		CodeRef:                exp.CodeRef,
		ConfigHash:             exp.ConfigHash,
		DataRef:                exp.DataRef,
		Job:                    exp.Job,
		Image:                  exp.Image,
		ImagePull:              pull,
		Launch:                 domain.LaunchScript,
		AcceleratorType:        exp.AcceleratorType,
		AcceleratorCount:       exp.AcceleratorCount,
		EstimatedDurationHours: exp.EstimatedDurationHours,
		CapacityTier:           exp.CapacityTier,
		SubmittedAt:            exp.SubmittedAt,
		Data: &clusterapi.DataAccess{
			URI:             s.dataStore.URI(objectstore.JobPrefix(exp.PlatformExperimentID, exp.AgentID, exp.ID)),
			Shared:          s.dataStore.URI(objectstore.PlatformExperimentPrefix(exp.PlatformExperimentID)),
			Endpoint:        s.dataStore.Endpoint,
			Region:          s.dataStore.Region,
			AccessKeyID:     exp.DataKey.AccessKeyID,
			SecretAccessKey: exp.DataKey.SecretAccessKey,
		},
		Token:        token,
		GitURL:       domain.GitProxyURL(s.proxyURL, exp.PlatformExperimentID),
		AttemptCount: exp.AttemptCount,
	}
	job.Job.SecretEnv = exp.SecretEnv
	if exp.ResolvedJob != nil {
		resolved := *exp.ResolvedJob
		resolved.SecretEnv = exp.SecretEnv
		job.ResolvedJob = &resolved
	}
	return job, nil
}
