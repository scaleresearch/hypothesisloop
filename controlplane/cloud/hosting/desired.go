package hosting

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/contract/hostingapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

// instance is one entry of the platform's list under the name the provider will hold it by.
type instance struct {
	name  string
	image string
	env   map[string]string
}

// desired is one answer of the platform, keyed by full instance name: what should run, and
// what has just stopped being wanted and is left to end by itself.
type desired struct {
	instances map[string]instance
	ending    map[string]bool
	// image is the platform's own image.
	image string
}

// read turns an answer into the pass's view of it under this launcher's prefix, or refuses the
// whole answer. Nothing in it is repaired or skipped: a list understood in part would have the
// launcher destroy the agents it did not understand.
func (l *Launcher) read(answer *hostingapi.Desired) (*desired, error) {
	want := &desired{instances: map[string]instance{}, ending: map[string]bool{}, image: answer.Image}
	for _, entry := range answer.Instances {
		name := l.prefix + entry.Name
		if entry.Name == "" || entry.Image == "" {
			return nil, fmt.Errorf("desired instance %q has no name or no image", entry.Name)
		}
		if err := provider.ValidateName(name); err != nil {
			return nil, fmt.Errorf("desired state: %w", err)
		}
		if strings.HasPrefix(entry.Name, poolSegment) {
			return nil, fmt.Errorf("desired instance %q starts with %q, which names this launcher's warm pool", entry.Name, poolSegment)
		}
		if _, twice := want.instances[name]; twice {
			return nil, fmt.Errorf("desired state names instance %q twice", entry.Name)
		}
		want.instances[name] = instance{name: name, image: entry.Image, env: entry.Env}
	}
	for _, short := range answer.Ending {
		name := l.prefix + short
		if short == "" {
			return nil, fmt.Errorf("desired state names an ending instance with no name")
		}
		if strings.HasPrefix(short, poolSegment) {
			return nil, fmt.Errorf("ending instance %q starts with %q, which names this launcher's warm pool", short, poolSegment)
		}
		if _, wanted := want.instances[name]; wanted {
			return nil, fmt.Errorf("desired state names instance %q as both running and ending", short)
		}
		want.ending[name] = true
	}
	return want, nil
}
