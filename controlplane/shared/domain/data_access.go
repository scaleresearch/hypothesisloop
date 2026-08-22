package domain

// DataAccess is where a job writes durable data, where it may read it, and the credentials for
// both. It is part of an experiment's desired state and nothing else: the control plane computes
// it from the platform experiment, the agent and the job id, hands it to the runtime, and never
// touches the bytes. It is never persisted — a stored copy of an address derived from three
// columns is a duplicate that can drift from them.
type DataAccess struct {
	// URI is this job's own prefix, writable. Shared is the platform experiment's prefix,
	// readable: any agent can load the checkpoint behind any claim, and no agent can overwrite
	// another's evidence.
	URI    string `json:"uri"`
	Shared string `json:"shared"`

	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`

	// The credentials are a session scoped to exactly the two grants above, minted per job. They
	// rotate on every reconcile pass, which is why WithoutCredentials exists.
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

// DataCredentialEnvNames are the environment variables carrying the rotating half of DataAccess.
// A runtime must leave these out of whatever it hashes to detect desired-state drift: a fresh
// session on the next reconcile pass is not the control plane asking for a different job, and
// hashing them deletes and recreates every running job every few seconds.
var DataCredentialEnvNames = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

// WithoutCredentials returns the stable half: where the job reads and writes, which is genuine
// desired state and must be hashed, without the session, which is not.
func (d *DataAccess) WithoutCredentials() *DataAccess {
	return &DataAccess{URI: d.URI, Shared: d.Shared, Endpoint: d.Endpoint, Region: d.Region}
}
