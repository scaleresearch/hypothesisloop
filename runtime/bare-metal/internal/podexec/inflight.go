package podexec

import (
	"context"
	"log"
	"maps"
)

// operation is one slow engine call running in its own goroutine for one job: pull and start,
// stop, or remove. The reconcile pass starts it and moves on; until it returns, the pass leaves
// that job alone and keeps counting the requests and devices recorded here as taken.
//
// The set is in memory only. That is safe because it describes nothing but this process's own
// goroutines: after a restart none are running, and the engine's containers are the whole truth.
type operation struct {
	// starting marks a pull and start. The job has no container yet, so the operation stands in
	// for one wherever the pass looks for the job.
	starting bool
	labels   map[string]string
	devices  []string
	cancel   context.CancelFunc
}

// failedPull is how a job's last pull ended, when it failed. A failed pull leaves nothing in the
// engine to read it back from, so it is kept here, and like the operations it describes only
// what this process did: after a restart the next pass pulls again and learns it anew.
type failedPull struct {
	labels map[string]string
	// reason is set for an image that can never be pulled and empty for a pull worth retrying.
	reason  string
	message string
}

// recordPull keeps the outcome of the pull that just returned for experimentID. A pull that
// succeeded or was cancelled says nothing against the image.
func (e *Executor) recordPull(ctx context.Context, experimentID string, labels map[string]string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil || ctx.Err() != nil {
		delete(e.failedPulls, experimentID)
		return
	}
	if e.failedPulls == nil {
		e.failedPulls = map[string]failedPull{}
	}
	e.failedPulls[experimentID] = failedPull{labels: labels, reason: pullFailureReason(err), message: err.Error()}
}

// pullFailures is the failed pulls remembered right now. Read it after operations: a pull that
// fails in between has by then recorded its failure, so the job is seen from one or the other.
func (e *Executor) pullFailures() map[string]failedPull {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.failedPulls)
}

func (e *Executor) forgetPullsExcept(desiredIDs map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	maps.DeleteFunc(e.failedPulls, func(id string, _ failedPull) bool { return !desiredIDs[id] })
}

// begin runs one operation for experimentID under ctx, which must outlive the call: cancelling
// it is how shutdown stops the operation. A job that already has one in flight gets no second.
func (e *Executor) begin(ctx context.Context, experimentID, what string, op operation, run func(context.Context) error) {
	ctx, op.cancel = context.WithCancel(ctx)
	e.mu.Lock()
	if _, busy := e.inFlight[experimentID]; busy {
		e.mu.Unlock()
		op.cancel()
		return
	}
	if e.inFlight == nil {
		e.inFlight = map[string]operation{}
	}
	e.inFlight[experimentID] = op
	e.workers.Add(1)
	e.mu.Unlock()

	go func() {
		defer e.workers.Done()
		defer op.cancel()
		if err := run(ctx); err != nil {
			log.Printf("podexec: %s %s: %v", what, experimentID, err)
		}
		e.mu.Lock()
		delete(e.inFlight, experimentID)
		e.mu.Unlock()
	}()
}

// operations is the set in flight right now. Read it before listing containers: an operation
// that ends in between has by then left its container in the state the list shows, so the job is
// counted from one or the other and never from neither.
func (e *Executor) operations() map[string]operation {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.inFlight)
}

// Wait blocks until every operation in flight has returned. Cancel the context the reconcile
// pass ran under first, or this waits for whole pulls and grace periods.
func (e *Executor) Wait() {
	e.workers.Wait()
}
