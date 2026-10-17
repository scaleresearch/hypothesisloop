package platformexperiments

import (
	"context"
	"sort"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
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

// DataUsage lists the platform experiment's whole prefix once and attributes each object to the agent
// segment of its key — one request to the store, not one per agent.
func (s *Service) DataUsage(ctx context.Context, platformExperimentID string) (*DataUsage, error) {
	prefix := objectstore.PlatformExperimentPrefix(platformExperimentID)
	objects, err := s.dataStore.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	bytesByAgent := map[string]int64{}
	countByAgent := map[string]int{}
	out := &DataUsage{PlatformExperimentID: platformExperimentID, MaxBytesPerAgent: s.maxDataBytesPerAgent, ByAgent: []AgentDataCost{}}
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
