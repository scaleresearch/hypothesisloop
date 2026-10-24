package compute

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
)

// instance is a rented instance read back through its name. The name is the only thing a
// provider stores for us, so it carries everything a later pass has to know: which job, which
// attempt of it and which of its ranks the instance runs, and which slot it occupies.
type instance struct {
	provider.Instance
	experimentID string
	attempt      int
	rank, ranks  int
	shape        string
	index        int
}

// instanceName is hl-job-<namespace>-<experiment id>-a<attempt>-r<rank>of<ranks>-<shape>-<index>.
// The attempt makes the name of a requeued job's instance differ from the one it replaces, which
// is the fence between them.
func (e *Executor) instanceName(experimentID string, attempt, rank, ranks int, shape string, index int) string {
	return fmt.Sprintf("%s%s-a%d-r%dof%d-%s", e.prefix, experimentID, attempt, rank, ranks, slotName(shape, index))
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
	n := len(parts)
	if n < 5 {
		return instance{}, false
	}
	index, indexErr := strconv.Atoi(parts[n-1])
	parsed = instance{shape: parts[n-2], index: index}
	_, rankErr := fmt.Sscanf(parts[n-3], "r%dof%d", &parsed.rank, &parsed.ranks)
	attempt, attemptErr := strconv.Atoi(strings.TrimPrefix(parts[n-4], "a"))
	if indexErr != nil || rankErr != nil || attemptErr != nil || parsed.rank < 0 || parsed.rank >= parsed.ranks {
		return instance{}, false
	}
	parsed.experimentID, parsed.attempt = strings.Join(parts[:n-4], "-"), attempt
	if parsed.experimentID == "" || e.instanceName(parsed.experimentID, parsed.attempt, parsed.rank, parsed.ranks, parsed.shape, parsed.index) != name {
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
	// demand is Executor.demanded's answer for this view, market Executor.rentable's and
	// networks Executor.networked's, each nil until it is asked.
	demand   *demand
	market   *market
	networks map[string]onOffer
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
	if err != nil {
		return nil, fmt.Errorf("cloudcompute: list instances: %w", err)
	}
	// A job is never paused: one found paused is one that does not run.
	listed = provider.PausedAsStarting(listed)
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

// gang returns the ranks of experimentID's highest attempt, lowest rank first, the lowest id of
// each rank's instances, which ReapTerminal keeps. A status read knows no slots, so it relies on
// CreateWorkload never renting an attempt in two placements.
func (v *view) gang(experimentID string) []instance {
	own := v.of(experimentID)
	newest := -1
	for _, i := range own {
		newest = max(newest, i.attempt)
	}
	var ranks []instance
	seen := map[int]bool{}
	for _, i := range own {
		if i.attempt == newest && !seen[i.rank] {
			seen[i.rank] = true
			ranks = append(ranks, i)
		}
	}
	sort.SliceStable(ranks, func(a, b int) bool { return ranks[a].rank < ranks[b].rank })
	return ranks
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
	line, reported := clusterapi.FindExitLine(read.lines)
	switch {
	case !reported:
		return endingUnknown, 0, nil
	case line.Code == 0:
		return endingSucceeded, 0, nil
	}
	return endingFailed, line.Code, nil
}
