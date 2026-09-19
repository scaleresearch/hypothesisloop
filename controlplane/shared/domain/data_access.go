package domain

// DataAccess is where a job writes durable data, where it may read it, and the key for both. It
// is part of an experiment's desired state and nothing else: the control plane assembles it from
// the platform experiment, the agent, the job id and the job's stored key, hands it to the
// runtime, and never touches the bytes.
type DataAccess struct {
	// URI is this job's own prefix, writable. Shared is the platform experiment's prefix,
	// readable: any agent can load the checkpoint behind any claim, and no agent can overwrite
	// another's evidence.
	URI    string `json:"uri"`
	Shared string `json:"shared"`

	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`

	// The job's own permanent key, scoped by the store to exactly the two grants above.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// DataKey is one job's permanent object-store key, never shared with another job.
type DataKey struct {
	AccessKeyID     string
	SecretAccessKey string
}
