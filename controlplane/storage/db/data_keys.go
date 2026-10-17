package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EndedDataKey is an object-store key whose holder's access is over: a job that ended longer ago
// than its workload may outlive it, named by ExperimentID, or an agent whose run ended longer
// than its wind-down ago, named by PlatformExperimentID and AgentID.
type EndedDataKey struct {
	ExperimentID         string
	PlatformExperimentID string
	AgentID              string
	AccessKeyID          string
}

// DataKeyGrace is how long a holder keeps its key past its end. Job covers an evicted job's
// checkpoint window and its container's shutdown, during which it still writes; Agent is the
// agent's wind-down.
type DataKeyGrace struct {
	Job   time.Duration
	Agent time.Duration
}

// endedJobKeySQL and endedAgentKeySQL hold for a row of experiments and of experiment_signups s
// whose key EndedDataKeys lists, its grace in seconds the parameter grace.
func endedJobKeySQL(grace string) string {
	return `data_access_key_id <> '' AND status IN ` + endedSQL + ` AND COALESCE(ended_at, updated_at) <= now() - make_interval(secs => ` + grace + `)`
}

func endedAgentKeySQL(grace string) string {
	return `s.data_access_key_id <> '' AND ` + runEndedAtSQL + ` <= now() - make_interval(secs => ` + grace + `)`
}

// EndedDataKeys lists the keys still recorded for jobs and agents that ended more than their grace
// ago. A key is recorded until DropDataKey took it out of the store.
func (s *Store) EndedDataKeys(ctx context.Context, grace DataKeyGrace) ([]EndedDataKey, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT id, '', '', data_access_key_id FROM experiments WHERE `+endedJobKeySQL("$1")+`
UNION ALL
SELECT '', s.platform_experiment_id, s.agent_id, s.data_access_key_id FROM experiment_signups s WHERE `+endedAgentKeySQL("$2"),
		grace.Job.Seconds(), grace.Agent.Seconds())
	if err != nil {
		return nil, fmt.Errorf("data_keys.EndedDataKeys: %w", err)
	}
	defer rows.Close()
	var keys []EndedDataKey
	for rows.Next() {
		var k EndedDataKey
		if err := rows.Scan(&k.ExperimentID, &k.PlatformExperimentID, &k.AgentID, &k.AccessKeyID); err != nil {
			return nil, fmt.Errorf("data_keys.EndedDataKeys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("data_keys.EndedDataKeys: %w", err)
	}
	return keys, nil
}

// DropDataKey takes key out of the store with deleteKey and then out of its row, while the row
// is locked and still holds it as ended, so a reopened run keeps its agents' keys. A key whose
// deletion fails stays recorded, and the next pass takes it again.
func (s *Store) DropDataKey(ctx context.Context, key EndedDataKey, grace DataKeyGrace, deleteKey func(ctx context.Context, accessKeyID string) error) error {
	return s.write(ctx, func(tx *Write) error {
		var held string
		var err error
		if key.ExperimentID != "" {
			err = tx.QueryRow(ctx, `SELECT data_access_key_id FROM experiments WHERE id = $1 AND `+endedJobKeySQL("$2")+` FOR UPDATE`,
				key.ExperimentID, grace.Job.Seconds()).Scan(&held)
		} else {
			err = tx.QueryRow(ctx, `
SELECT s.data_access_key_id FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.agent_id = $2 AND `+endedAgentKeySQL("$3")+` FOR UPDATE OF s`,
				key.PlatformExperimentID, key.AgentID, grace.Agent.Seconds()).Scan(&held)
		}
		if errors.Is(err, pgx.ErrNoRows) || err == nil && held != key.AccessKeyID {
			return nil
		}
		if err != nil {
			return fmt.Errorf("data_keys.DropDataKey: %w", err)
		}
		if err := deleteKey(ctx, key.AccessKeyID); err != nil {
			return fmt.Errorf("data_keys.DropDataKey: delete %s: %w", key.AccessKeyID, err)
		}
		if key.ExperimentID != "" {
			_, err = tx.Exec(ctx, `UPDATE experiments SET data_access_key_id = '', data_secret_access_key = '' WHERE id = $1`, key.ExperimentID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE experiment_signups SET data_access_key_id = '', data_secret_access_key = '' WHERE platform_experiment_id = $1 AND agent_id = $2`,
				key.PlatformExperimentID, key.AgentID)
		}
		if err != nil {
			return fmt.Errorf("data_keys.DropDataKey: clear: %w", err)
		}
		return nil
	})
}
