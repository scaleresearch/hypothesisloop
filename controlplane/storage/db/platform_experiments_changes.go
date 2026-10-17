package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// statusMoved is what every write that moves the status of a platform experiment sets beside it:
// what its coordinator was told it waits for and the researchers held said of the status that
// was.
const statusMoved = `coordinator_reason = NULL, researchers_held = NULL`

// moveRun runs q, a write of at most one platform_experiments row that moves its status, and
// announces the status it moved to, reporting whether there was a row.
func moveRun(ctx context.Context, w *Write, q string, args ...any) (bool, error) {
	var id, status, closeMessage string
	err := w.QueryRow(ctx, q+` RETURNING id, status, close_message`, args...).Scan(&id, &status, &closeMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	w.runStatus(id, status, closeMessage)
	return true, nil
}

// runStatus announces the status a platform experiment moved to, closed with what whoever closed
// it said of why.
func (w *Write) runStatus(id, status, closeMessage string) {
	detail := ""
	if status == string(domain.PlatformExpClosed) {
		detail = closeMessage
	}
	w.runChange(id, domain.EventPlatformExperimentStatus, status, detail)
}

func (w *Write) runChange(id, kind, value, detail string) {
	w.Change(domain.Change{Kind: kind, Subject: id, Value: value, Detail: detail, PlatformExperimentID: id})
}
