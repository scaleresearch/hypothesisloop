package cloudexec

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const namePrefix = "hl-"

// instance is a rented instance read back through its name. The name is the only thing a
// provider stores for us, and this runtime keeps nothing between passes, so the name carries
// everything a later pass has to know: which job and which attempt of it the instance runs, and
// which slot of which shape it occupies.
type instance struct {
	provider.Instance
	experimentID string
	attempt      int
	shape        string
	slot         int
}

// instanceName is hl-<experiment id>-a<attempt>-<shape>-<slot>. The attempt makes the name of a
// requeued job's instance differ from the one it replaces, which is the fence between them.
func instanceName(experimentID string, attempt int, shape string, slot int) string {
	return fmt.Sprintf("%s%s-a%d-%s", namePrefix, experimentID, attempt, nodeName(shape, slot))
}

func nodeName(shape string, slot int) string {
	return fmt.Sprintf("%s-%d", shape, slot)
}

// parseInstanceName reads a name from the right, since an experiment id may itself hold dashes.
func parseInstanceName(name string) (parsed instance, ok bool) {
	rest, found := strings.CutPrefix(name, namePrefix)
	if !found {
		return instance{}, false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 4 {
		return instance{}, false
	}
	n := len(parts)
	slot, slotErr := strconv.Atoi(parts[n-1])
	attempt, attemptErr := strconv.Atoi(strings.TrimPrefix(parts[n-3], "a"))
	if slotErr != nil || attemptErr != nil {
		return instance{}, false
	}
	parsed = instance{
		experimentID: strings.Join(parts[:n-3], "-"),
		attempt:      attempt,
		shape:        parts[n-2],
		slot:         slot,
	}
	if parsed.experimentID == "" || instanceName(parsed.experimentID, parsed.attempt, parsed.shape, parsed.slot) != name {
		return instance{}, false
	}
	return parsed, true
}

func (i instance) node() string { return nodeName(i.shape, i.slot) }

// live is true until the command has ended. A live instance is what the reconcile pass compares
// with desired state; an exited one only waits to be read and reaped.
func (i instance) live() bool { return i.State != provider.StateExited }

// list reads every instance of this account that carries one of our names.
func (e *Executor) list(ctx context.Context) ([]instance, error) {
	listed, err := e.provider.List(ctx, namePrefix)
	if err != nil {
		return nil, fmt.Errorf("cloudexec: list instances: %w", err)
	}
	instances := make([]instance, 0, len(listed))
	for _, l := range listed {
		parsed, ok := parseInstanceName(l.Name)
		if !ok {
			// One instance that is not a job's must not stop the pass for every job that is
			// (important.md #19).
			log.Printf("cloudexec: skipping instance %q, its name is not a job's", l.Name)
			continue
		}
		parsed.Instance = l
		instances = append(instances, parsed)
	}
	return instances, nil
}

// of returns experimentID's instances.
func of(instances []instance, experimentID string) []instance {
	var own []instance
	for _, i := range instances {
		if i.experimentID == experimentID {
			own = append(own, i)
		}
	}
	return own
}

// newest returns experimentID's instance of the highest attempt: the one the control plane is
// waiting on when a requeue has left an older one behind.
func newest(instances []instance, experimentID string) (instance, bool) {
	var found instance
	ok := false
	for _, i := range of(instances, experimentID) {
		if !ok || i.attempt > found.attempt {
			found, ok = i, true
		}
	}
	return found, ok
}
