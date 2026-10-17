package compute

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/exitline"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

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
//
// unnamed are the instances under this namespace's prefix whose names read as no job's. The
// prefix makes them this runtime's and no job can be desired for them, so ReapTerminal destroys
// them.
type view struct {
	instances []instance
	unnamed   []instance
	outputs   map[string]output
	// rentable is Executor.rentable's answer for this view, nil until it is asked.
	rentable map[string]onOffer
	// unstarting are the hosts of the instances this read found overdue. The pass destroys those
	// instances, and what it rents is not rented on their hosts. Nothing carries a host into
	// the next pass: once its instance is destroyed no list names it.
	unstarting []string
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
	// A job is never paused: one found paused is one that does not run.
	listed = provider.PausedAsStarting(listed)
	if err != nil {
		return nil, fmt.Errorf("cloudcompute: list instances: %w", err)
	}
	v := &view{outputs: map[string]output{}}
	for _, l := range listed {
		parsed, ok := e.parseInstanceName(l.Name)
		parsed.Instance = l
		if !ok {
			v.unnamed = append(v.unnamed, parsed)
			continue
		}
		v.instances = append(v.instances, parsed)
	}
	sort.SliceStable(v.instances, func(a, b int) bool { return v.instances[a].ID < v.instances[b].ID })
	for _, i := range v.instances {
		if e.overdue(i) && i.Host != "" && !slices.Contains(v.unstarting, i.Host) {
			v.unstarting = append(v.unstarting, i.Host)
		}
	}
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
//
// A status read has no desired state to tell it which slot the job is pinned to. CreateWorkload
// never rents an attempt while an instance of it stands in another slot, so there is one
// placement of an attempt to report at a time.
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
	differs := func(i instance) bool { return i.ID != id }
	v.instances = keep(v.instances, differs)
	v.unnamed = keep(v.unnamed, differs)
}

func keep(instances []instance, include func(instance) bool) []instance {
	kept := instances[:0]
	for _, i := range instances {
		if include(i) {
			kept = append(kept, i)
		}
	}
	return kept
}

// output reads the end of i's output, once per view however many questions are answered from it.
func (e *Executor) output(ctx context.Context, v *view, i instance) output {
	if read, done := v.outputs[i.ID]; done {
		return read
	}
	lines, err := e.provider.Logs(ctx, i.ID, max(agentexec.LogTailLines, exitLineTail))
	if err != nil {
		err = fmt.Errorf("cloudcompute: read output of instance %s: %w", i.Name, err)
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
	// endingUnknown: the wrapper's exit line cannot be found. Either the output is readable and
	// has none, because the wrapper died with its machine or never ran, or the output cannot be
	// read, in which case readErr says why.
	endingUnknown ending = iota
	endingSucceeded
	endingFailed
)

func (e *Executor) ending(ctx context.Context, v *view, i instance) (how ending, code int, readErr error) {
	read := e.output(ctx, v, i)
	if read.err != nil {
		return endingUnknown, 0, read.err
	}
	code, reported := exitline.Find(read.lines)
	switch {
	case !reported:
		return endingUnknown, 0, nil
	case code == 0:
		return endingSucceeded, 0, nil
	}
	return endingFailed, code, nil
}
