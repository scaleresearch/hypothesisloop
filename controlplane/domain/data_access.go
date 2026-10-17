package domain

import "github.com/scaleresearch/hypothesisloop/contract/clusterapi"

// DataAccess is part of an experiment's desired state and nothing else: the control plane
// assembles it from the platform experiment, the agent, the job id and the job's stored key, hands
// it to the runtime, and never touches the bytes. An agent asking for its own key gets the same
// shape, with URI its agent prefix.
type DataAccess = clusterapi.DataAccess

// DataKey is one job's permanent object-store key, never shared with another job.
type DataKey struct {
	AccessKeyID     string
	SecretAccessKey string
}
