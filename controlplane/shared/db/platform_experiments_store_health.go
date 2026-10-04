package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// RunHealth returns how each agent of every open or running platform experiment is getting on,
// one statement so every agent is described at the same moment. An agent is timed from when it
// could first submit: a researcher from the run's start or, joining later, its own signup; any
// other agent from its signup. A job ended at the transaction that set its status, whose time
// event_times holds.
func (s *PlatformExperimentsStore) RunHealth(ctx context.Context) ([]domain.AgentHealth, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT s.platform_experiment_id, s.agent_id, s.role,
       EXTRACT(EPOCH FROM min(e.created_at) - st.start)::float8,
       EXTRACT(EPOCH FROM min(t.occurred_at) - st.start)::float8,
       count(e.id) FILTER (WHERE e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING')),
       s.instance_restarts,
       (SELECT count(*) FROM agent_messages m
         WHERE m.platform_experiment_id = s.platform_experiment_id AND m.agent_id = s.agent_id AND m.sender = 'coordinator')
FROM experiment_signups s
JOIN platform_experiments p ON p.id = s.platform_experiment_id
CROSS JOIN LATERAL (SELECT CASE WHEN s.role = 'competitor' THEN GREATEST(p.started_at, s.signed_up_at) ELSE s.signed_up_at END AS start) st
LEFT JOIN experiments e ON e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id
LEFT JOIN event_times t ON t.xid = e.status_xid AND e.status = 'COMPLETED'
WHERE p.status IN ('open', 'running')
GROUP BY s.platform_experiment_id, s.agent_id, s.role, st.start, s.instance_restarts`)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.RunHealth: %w", err)
	}
	defer rows.Close()
	var agents []domain.AgentHealth
	for rows.Next() {
		var a domain.AgentHealth
		if err := rows.Scan(&a.PlatformExperimentID, &a.AgentID, &a.Role, &a.FirstJobSeconds, &a.FirstCompletedSeconds, &a.JobsInFlight, &a.Restarts, &a.CoordinatorMessages); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.RunHealth: %w", err)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.RunHealth: %w", err)
	}
	return agents, nil
}
