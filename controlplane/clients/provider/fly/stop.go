package fly

import (
	"context"
	"net/http"
)

// Stop signals the Machine's main process, the supervisor, which passes the first SIGTERM on to
// the command and ignores the rest. Nothing follows the signal: the Machine stops when the
// command has ended in its own time.
//
// The API answers 200 for a Machine that has stopped, 404 for one that is gone and 412 for one
// that has no process to signal yet or any more. A Machine still starting is signalled on a
// later pass, since Stop is repeated while the instance runs.
func (p *Provider) Stop(ctx context.Context, id string) error {
	app, machineID, err := p.splitID(id)
	if err != nil {
		return err
	}
	_, err = p.call(ctx, http.MethodPost, p.machinePath(app, machineID, "/signal"), map[string]string{"signal": "SIGTERM"}, nil)
	if hasStatus(err, http.StatusNotFound, http.StatusPreconditionFailed) {
		return nil
	}
	return err
}
