package fly

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

var _ provider.Pauser = (*Provider)(nil)

// Pause suspends the Machine: the vendor snapshots its memory and stops billing for compute, and
// the root filesystem stays as it is. The vendor's stop is not used, because a start after it
// boots the image's filesystem again whatever happens.
//
// What real Machines taught:
//
//   - The vendor's documentation gives 2 GiB of memory as the most it suspends. A shared Machine
//     of 2 CPUs and 4 GiB, the size agents ran in then, suspended in 7 seconds and resumed the same
//     process. A refusal, should the limit be enforced, is returned as it came.
//   - The snapshot is the vendor's to drop (a host migration, maintenance). A Machine that lost
//     it starts cold on Resume: the supervisor and the command run again from the start, on the
//     image's filesystem. So Resume continues the same process when the vendor kept the
//     snapshot and starts the command again when it did not, and nothing tells which beforehand.
//
// The API answers 404 for an id it never had and 409 for a Machine in a state it cannot suspend
// from, which a destroyed one is for a while. A Machine that is gone is not an error here, so a
// 409 is followed by a read of the Machine.
func (p *Provider) Pause(ctx context.Context, id string) error {
	path := p.machinesPath("/" + url.PathEscape(id))
	_, err := p.call(ctx, http.MethodPost, path+"/suspend", nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	if hasStatus(err, http.StatusConflict) {
		var found machine
		_, readErr := p.call(ctx, http.MethodGet, path, nil, &found)
		if hasStatus(readErr, http.StatusNotFound) || (readErr == nil && (found.State == "destroyed" || found.State == "destroying")) {
			return nil
		}
	}
	return err
}

// Resume starts the Machine, which the vendor does from the snapshot when it has one. Starting
// a Machine that runs is answered 200.
//
// The vendor starts a stopped Machine as readily as a suspended one, and that runs a command
// that has ended a second time. So the Machine is read first, and one whose command ended is
// refused: it is exited, and a caller that wants its command again rents another.
func (p *Provider) Resume(ctx context.Context, id string) error {
	path := p.machinesPath("/" + url.PathEscape(id))
	var found machine
	_, err := p.call(ctx, http.MethodGet, path, nil, &found)
	if hasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("fly: machine %s is gone and cannot be resumed: %w", id, err)
	}
	if err != nil {
		return err
	}
	if found.State == "stopped" || found.State == "failed" {
		return fmt.Errorf("fly: machine %s is %s and cannot be resumed: its command has ended, and a start would run it again", id, found.State)
	}
	_, err = p.call(ctx, http.MethodPost, path+"/start", nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("fly: machine %s is gone and cannot be resumed: %w", id, err)
	}
	return err
}
