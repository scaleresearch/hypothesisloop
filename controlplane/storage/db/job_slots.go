package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// moveSlotsToJobs brings a database stored before a job held its slots in rank order onto them:
// a resolved job that names its slot in its node selector names it as the slot of its one node
// (domain.JobSpec.Slots) and no longer in its node selector. It changes nothing on a database that
// holds none.
func moveSlotsToJobs(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `UPDATE experiments
SET resolved_job_spec = jsonb_set(resolved_job_spec #- ARRAY['node_selector', $1::text], '{slots}', jsonb_build_array(resolved_job_spec->'node_selector'->$1::text))
WHERE jsonb_typeof(resolved_job_spec->'node_selector') = 'object' AND resolved_job_spec->'node_selector' ? $1::text`, domain.NodeSlotLabel); err != nil {
		return fmt.Errorf("move slots to jobs: %w", err)
	}
	return nil
}
