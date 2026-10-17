package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// RunHealth returns how each agent and hosted coordinator of every open or running platform
// experiment is getting on, one statement so every one is described at the same moment. An agent
// is timed from when it could first submit: a researcher from the run's start or, joining later,
// its own signup; any other agent from its signup. A job ended at its ended_at. Restarts are the
// instance counters, which count every machine asked for again over the run, whatever launch it
// was under.
func (s *Store) RunHealth(ctx context.Context) ([]domain.AgentHealth, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT s.platform_experiment_id, s.agent_id, s.role,
       EXTRACT(EPOCH FROM min(e.created_at) - st.start)::float8,
       EXTRACT(EPOCH FROM min(e.ended_at) FILTER (WHERE e.status = 'COMPLETED') - st.start)::float8,
       count(e.id) FILTER (WHERE e.status IN ('QUEUED', 'SUBMITTED', 'RUNNING')),
       COALESCE(i.restarts, 0),
       (SELECT count(*) FROM agent_messages m
         WHERE m.platform_experiment_id = s.platform_experiment_id AND m.agent_id = s.agent_id AND m.sender = 'coordinator')
FROM experiment_signups s`+signupInstanceJoin+`
JOIN platform_experiments p ON p.id = s.platform_experiment_id
CROSS JOIN LATERAL (SELECT CASE WHEN s.role = 'competitor' THEN GREATEST(p.started_at, s.signed_up_at) ELSE s.signed_up_at END AS start) st
LEFT JOIN experiments e ON e.platform_experiment_id = s.platform_experiment_id AND e.agent_id = s.agent_id
WHERE p.status IN ('open', 'running')
GROUP BY s.platform_experiment_id, s.agent_id, s.role, st.start, i.restarts
UNION ALL
SELECT p.id, '', $1::text, NULL, NULL, 0, COALESCE(ci.restarts, 0), 0
FROM platform_experiments p
LEFT JOIN hosted_instances ci ON ci.platform_experiment_id = p.id AND ci.agent_id = ''
WHERE p.status IN ('open', 'running') AND p.coordinator_hosted`, domain.CoordinatorHealthRole)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.RunHealth: %w", err)
	}
	defer rows.Close()
	agents := []domain.AgentHealth{}
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
