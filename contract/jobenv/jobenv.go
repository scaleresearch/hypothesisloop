// Package jobenv names the environment a job's process starts with, which every runtime sets and
// a job's code, hl and the platform's own tools inside a job read.
package jobenv

// The object store a job writes its durable data to: the job's own key, the store's address, and
// the two prefixes the key opens. hl data-key prints the same names for an agent's own key.
const (
	AccessKeyID     = "AWS_ACCESS_KEY_ID"
	SecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	Endpoint        = "AWS_ENDPOINT_URL"
	EndpointS3      = "AWS_ENDPOINT_URL_S3"
	Region          = "AWS_REGION"
	// DataURI is the job's own prefix, writable; DataShared its platform experiment's, readable.
	DataURI    = "HYPOTHESISLOOP_DATA_URI"
	DataShared = "HYPOTHESISLOOP_DATA_SHARED"
)

// What the job is and what it runs on.
const (
	ExperimentID     = "HYPOTHESISLOOP_EXPERIMENT_ID"
	AgentID          = "HYPOTHESISLOOP_AGENT_ID"
	ProjectID        = "HYPOTHESISLOOP_PROJECT_ID"
	CodeRef          = "HYPOTHESISLOOP_CODE_REF"
	ConfigHash       = "HYPOTHESISLOOP_CONFIG_HASH"
	DataRef          = "HYPOTHESISLOOP_DATA_REF"
	APIURL           = "HYPOTHESISLOOP_API_URL"
	AcceleratorType  = "HYPOTHESISLOOP_ACCELERATOR_TYPE"
	AcceleratorCount = "HYPOTHESISLOOP_ACCELERATOR_COUNT"
	DurationSeconds  = "HYPOTHESISLOOP_DURATION_SECONDS"
	Attempt          = "HYPOTHESISLOOP_ATTEMPT"
	// CPUs is the whole cores the job's cpu allows, set with OMPNumThreads only when the job
	// states its cpu.
	CPUs          = "HYPOTHESISLOOP_CPUS"
	OMPNumThreads = "OMP_NUM_THREADS"
	TraceParent   = "TRACEPARENT"
	// Token is the job's platform token, which it pushes its metrics with.
	Token = "HYPOTHESISLOOP_TOKEN"
	// GitURL is the platform's git proxy address of the job's repository, which the launch
	// script fetches the job's commit from.
	GitURL = "HYPOTHESISLOOP_GIT_URL"
	// StopGrace is how many seconds the job's command runs on after SIGTERM before the launch
	// script kills it: the longer of the job's resolved shutdown grace and checkpoint window.
	StopGrace = "HYPOTHESISLOOP_STOP_GRACE"
)

// Where a node of a job that spans several stands among them.
const (
	WorldSize     = "HYPOTHESISLOOP_WORLD_SIZE"
	MasterAddr    = "HYPOTHESISLOOP_MASTER_ADDR"
	MasterPort    = "HYPOTHESISLOOP_MASTER_PORT"
	Rank          = "HYPOTHESISLOOP_RANK"
	Group         = "HYPOTHESISLOOP_GROUP"
	GroupRank     = "HYPOTHESISLOOP_GROUP_RANK"
	GroupReplicas = "HYPOTHESISLOOP_GROUP_REPLICAS"
	RankOffset    = "HYPOTHESISLOOP_RANK_OFFSET"
)
