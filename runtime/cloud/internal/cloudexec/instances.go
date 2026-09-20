package cloudexec

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

// jobPrefix starts the name of every job's instance. What follows is the namespace, so the full
// prefix of one executor's instances is never the prefix of another's or of anything else rented
// on the account. "hl-agent-<namespace>-" is kept for the instances agents themselves run in.
const jobPrefix = "hl-job-"

// instance is a rented instance read back through its name. The name is the only thing a
// provider stores for us, so it carries everything a later pass has to know: which job and which
// attempt of it the instance runs, and which slot it occupies.
type instance struct {
	provider.Instance
	experimentID string
	attempt      int
	shape        string
	index        int
}

// instanceName is hl-job-<namespace>-<experiment id>-a<attempt>-<shape>-<index>. The attempt
// makes the name of a requeued job's instance differ from the one it replaces, which is the fence
// between them.
func (e *Executor) instanceName(experimentID string, attempt int, shape string, index int) string {
	return fmt.Sprintf("%s%s-a%d-%s", e.prefix, experimentID, attempt, slotName(shape, index))
}

// slotName is the node name the control plane knows a slot by.
func slotName(shape string, index int) string {
	return fmt.Sprintf("%s-%d", shape, index)
}

// parseInstanceName reads a name from the right, since an experiment id may itself hold dashes.
func (e *Executor) parseInstanceName(name string) (parsed instance, ok bool) {
	rest, found := strings.CutPrefix(name, e.prefix)
	if !found {
		return instance{}, false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 4 {
		return instance{}, false
	}
	n := len(parts)
	index, indexErr := strconv.Atoi(parts[n-1])
	attempt, attemptErr := strconv.Atoi(strings.TrimPrefix(parts[n-3], "a"))
	if indexErr != nil || attemptErr != nil {
		return instance{}, false
	}
	parsed = instance{
		experimentID: strings.Join(parts[:n-3], "-"),
		attempt:      attempt,
		shape:        parts[n-2],
		index:        index,
	}
	if parsed.experimentID == "" || e.instanceName(parsed.experimentID, parsed.attempt, parsed.shape, parsed.index) != name {
		return instance{}, false
	}
	return parsed, true
}

func (i instance) slot() string { return slotName(i.shape, i.index) }

// live is true until the command has ended. A live instance is what the reconcile pass compares
// with desired state; an exited one only waits to be read and reaped.
func (i instance) live() bool { return i.State != provider.StateExited }

// view is one read of the account: the instances of this namespace, lowest id first, and what
// was read of their output since. A pass that creates or destroys an instance keeps the view in
// step, so a later decision in the same pass sees it.
type view struct {
	instances []instance
	outputs   map[string]output
}

// output is the end of one instance's output, or why it could not be read.
type output struct {
	lines []string
	err   error
}

// current returns the pass's view, or a fresh read for an executor that is not pass-scoped.
func (e *Executor) current(ctx context.Context) (*view, error) {
	if e.pass != nil {
		return e.pass, nil
	}
	return e.read(ctx)
}

func (e *Executor) read(ctx context.Context) (*view, error) {
	listed, err := e.provider.List(ctx, e.prefix)
	if err != nil {
		return nil, fmt.Errorf("cloudexec: list instances: %w", err)
	}
	v := &view{outputs: map[string]output{}}
	for _, l := range listed {
		parsed, ok := e.parseInstanceName(l.Name)
		if !ok {
			// One name that does not read as a job's must not stop the pass for every job.
			log.Printf("cloudexec: leaving instance %q alone, its name is not one of this namespace's jobs", l.Name)
			continue
		}
		parsed.Instance = l
		v.instances = append(v.instances, parsed)
	}
	sort.SliceStable(v.instances, func(a, b int) bool { return v.instances[a].ID < v.instances[b].ID })
	return v, nil
}

// of returns experimentID's instances.
func (v *view) of(experimentID string) []instance {
	var own []instance
	for _, i := range v.instances {
		if i.experimentID == experimentID {
			own = append(own, i)
		}
	}
	return own
}

// newest returns experimentID's instance of the highest attempt: the one the control plane is
// waiting on when a requeue has left an older one behind. Of two instances of that attempt,
// which a vendor's lagging list or a create that failed after it succeeded can leave, it is the
// one with the lowest id, the one ReapTerminal keeps.
func (v *view) newest(experimentID string) (instance, bool) {
	var found instance
	ok := false
	for _, i := range v.of(experimentID) {
		if !ok || i.attempt > found.attempt {
			found, ok = i, true
		}
	}
	return found, ok
}

func (v *view) add(i instance) {
	v.instances = append(v.instances, i)
}

func (v *view) remove(id string) {
	kept := v.instances[:0]
	for _, i := range v.instances {
		if i.ID != id {
			kept = append(kept, i)
		}
	}
	v.instances = kept
}

// output reads the end of i's output, once per view however many questions are answered from it.
func (e *Executor) output(ctx context.Context, v *view, i instance) output {
	if read, done := v.outputs[i.ID]; done {
		return read
	}
	lines, err := e.provider.Logs(ctx, i.ID, max(e.logTailLines, exitLineTail))
	if err != nil {
		err = fmt.Errorf("cloudexec: read output of instance %s: %w", i.Name, err)
	}
	v.outputs[i.ID] = output{lines: lines, err: err}
	return v.outputs[i.ID]
}

// exitLineTail is the least that is read of an exited instance's output. The wrapper writes the
// exit line last; the margin is for output that reaches the vendor's log after it.
const exitLineTail = 50

// ending is how an exited instance's command ended.
type ending int

const (
	// endingUnread: the output could not be read, so how the command ended is not known yet.
	endingUnread ending = iota
	endingSucceeded
	endingFailed
	// endingUnreported: the output is readable and holds no exit line. The wrapper did not get
	// to write one: the vendor took the machine back, killed it for memory, or it never ran.
	endingUnreported
)

func (e *Executor) ending(ctx context.Context, v *view, i instance) (ending, int, error) {
	read := e.output(ctx, v, i)
	if read.err != nil {
		return endingUnread, 0, read.err
	}
	code, reported := exitline.Find(read.lines)
	switch {
	case !reported:
		return endingUnreported, 0, nil
	case code == 0:
		return endingSucceeded, 0, nil
	}
	return endingFailed, code, nil
}
