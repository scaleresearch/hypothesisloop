package platformexperiments

import (
	"context"
	"sort"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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

// AgentDataAccess is the agent's key and the two prefixes it opens. The key is created in the
// object store on the first call and read from the signup afterwards.
func (s *PlatformExperimentsService) AgentDataAccess(ctx context.Context, platformExperimentID, agentID string) (*domain.DataAccess, error) {
	key, err := s.store.AgentDataKey(ctx, platformExperimentID, agentID, func(ctx context.Context) (domain.DataKey, error) {
		key, err := objectstore.NewDataKey()
		if err != nil {
			return domain.DataKey{}, err
		}
		return key, s.dataStore.CreateAgentDataKey(ctx, key, platformExperimentID, agentID)
	})
	if err != nil {
		return nil, err
	}
	return &domain.DataAccess{
		URI:             s.dataStore.URI(objectstore.AgentPrefix(platformExperimentID, agentID)),
		Shared:          s.dataStore.URI(objectstore.PlatformExperimentPrefix(platformExperimentID)),
		Endpoint:        s.dataStore.Endpoint,
		Region:          s.dataStore.Region,
		AccessKeyID:     key.AccessKeyID,
		SecretAccessKey: key.SecretAccessKey,
	}, nil
}

// DataUsage lists the platform experiment's whole prefix once and attributes each object to the agent
// segment of its key — one request to the store, not one per agent.
func (s *PlatformExperimentsService) DataUsage(ctx context.Context, platformExperimentID string) (*DataUsage, error) {
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
