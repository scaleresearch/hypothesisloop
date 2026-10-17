package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EndedDataKey is the object-store key of a job that ended longer ago than its workload may
// outlive it.
type EndedDataKey struct {
	ExperimentID string
	AccessKeyID  string
}

// endedJobKeySQL holds for a row of experiments whose key EndedDataKeys lists, grace in seconds
// the parameter grace: an evicted job's checkpoint window and its container's shutdown, during
// which it still writes.
func endedJobKeySQL(grace string) string {
	return `data_access_key_id <> '' AND status IN ` + endedSQL + ` AND COALESCE(ended_at, updated_at) <= now() - make_interval(secs => ` + grace + `)`
}

// EndedDataKeys lists the keys still recorded for jobs that ended more than grace ago. A key is
// recorded until DropDataKey took it out of the store.
func (s *Store) EndedDataKeys(ctx context.Context, grace time.Duration) ([]EndedDataKey, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT id, data_access_key_id FROM experiments WHERE `+endedJobKeySQL("$1"), grace.Seconds())
	if err != nil {
		return nil, fmt.Errorf("data_keys.EndedDataKeys: %w", err)
	}
	defer rows.Close()
	var keys []EndedDataKey
	for rows.Next() {
		var k EndedDataKey
		if err := rows.Scan(&k.ExperimentID, &k.AccessKeyID); err != nil {
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
// is locked and still holds it as ended. A key whose deletion fails stays recorded, and the next
// pass takes it again.
func (s *Store) DropDataKey(ctx context.Context, key EndedDataKey, grace time.Duration, deleteKey func(ctx context.Context, accessKeyID string) error) error {
	return s.write(ctx, func(tx *Write) error {
		var held string
		err := tx.QueryRow(ctx, `SELECT data_access_key_id FROM experiments WHERE id = $1 AND `+endedJobKeySQL("$2")+` FOR UPDATE`,
			key.ExperimentID, grace.Seconds()).Scan(&held)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && held != key.AccessKeyID {
			return nil
		}
		if err != nil {
			return fmt.Errorf("data_keys.DropDataKey: %w", err)
		}
		if err := deleteKey(ctx, key.AccessKeyID); err != nil {
			return fmt.Errorf("data_keys.DropDataKey: delete %s: %w", key.AccessKeyID, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE experiments SET data_access_key_id = '', data_secret_access_key = '' WHERE id = $1`, key.ExperimentID); err != nil {
			return fmt.Errorf("data_keys.DropDataKey: clear: %w", err)
		}
		return nil
	})
}
