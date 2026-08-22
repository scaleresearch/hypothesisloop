package quota

import (
	"context"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
)

// DataUsage is what a platform experiment's durable data costs in bytes, and who is holding it.
// Read live from the object store on every request: the bytes are the only record of themselves,
// and the ceiling admission enforces is measured the same way, so the two can never disagree.
type DataUsage struct {
	PlatformExperimentID string          `json:"platform_experiment_id"`
	TotalBytes           int64           `json:"total_bytes"`
	MaxBytesPerAgent     int64           `json:"max_bytes_per_agent"`
	ByAgent              []AgentDataCost `json:"by_agent"`
}

// AgentDataCost is one agent's share of a platform experiment's stored bytes.
type AgentDataCost struct {
	AgentID     string `json:"agent_id"`
	Bytes       int64  `json:"bytes"`
	ObjectCount int    `json:"object_count"`
}

// DataUsageHandler serves the durable-data half of what a platform experiment costs.
type DataUsageHandler struct {
	dataStore        *objectstore.Client
	maxBytesPerAgent int64
}

// NewDataUsageHandler returns a handler reporting against client, with the same per-agent ceiling
// admission enforces so a caller can see how close it is to being refused.
func NewDataUsageHandler(client *objectstore.Client, maxBytesPerAgent int64) *DataUsageHandler {
	return &DataUsageHandler{dataStore: client, maxBytesPerAgent: maxBytesPerAgent}
}

// RegisterDataUsageHuma registers the durable-data usage operation on doc.
func RegisterDataUsageHuma(doc *apidocs.Doc, h *DataUsageHandler) {
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-platform-experiment-data-usage", Method: "GET", Path: "/platform-experiments/{id}/data-usage",
		Summary: "Get durable-data bytes held, per agent", Tags: []string{"platform-experiments"},
		Description: "Checkpoint and dataset bytes stored under this platform experiment, totalled and broken down by " +
			"agent, read live from the object store. max_bytes_per_agent is the ceiling admission refuses a submission " +
			"against (reason data_quota_exceeded); 0 means unlimited.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *DataUsage }, error) {
		usage, err := h.Usage(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *DataUsage }{Body: usage}, nil
	})
}

// Usage lists the platform experiment's whole prefix once and attributes each object to the agent
// segment of its key — one request to the store, not one per agent.
func (h *DataUsageHandler) Usage(ctx context.Context, platformExperimentID string) (*DataUsage, error) {
	prefix := objectstore.PlatformExperimentPrefix(platformExperimentID)
	objects, err := h.dataStore.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	bytesByAgent := map[string]int64{}
	countByAgent := map[string]int{}
	out := &DataUsage{PlatformExperimentID: platformExperimentID, MaxBytesPerAgent: h.maxBytesPerAgent, ByAgent: []AgentDataCost{}}
	for _, o := range objects {
		out.TotalBytes += o.SizeBytes
		agent, _, ok := strings.Cut(strings.TrimPrefix(o.Key, prefix), "/")
		if !ok {
			continue
		}
		bytesByAgent[agent] += o.SizeBytes
		countByAgent[agent]++
	}
	agents := make([]string, 0, len(bytesByAgent))
	for agent := range bytesByAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	for _, agent := range agents {
		out.ByAgent = append(out.ByAgent, AgentDataCost{AgentID: agent, Bytes: bytesByAgent[agent], ObjectCount: countByAgent[agent]})
	}
	return out, nil
}
