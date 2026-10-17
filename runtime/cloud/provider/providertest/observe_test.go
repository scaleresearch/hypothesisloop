package providertest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

// faulted records what a check holds against an adapter, in place of failing this test.
type faulted struct {
	testing.TB
	faults []string
}

func (f *faulted) Helper() {}

func (f *faulted) Errorf(format string, args ...any) {
	f.faults = append(f.faults, fmt.Sprintf(format, args...))
}

func TestEveryListedStateIsHeldToTheContract(t *testing.T) {
	for name, tc := range map[string]struct {
		states []provider.State
		fault  string
	}{
		"a whole life":                      {[]provider.State{provider.StateStarting, provider.StateRunning, provider.StateExited}, ""},
		"unreachable under a command":       {[]provider.State{provider.StateStarting, provider.StateRunning, provider.StateUnknown, provider.StateRunning}, ""},
		"unreachable before it started":     {[]provider.State{provider.StateUnknown, provider.StateStarting, provider.StateRunning}, ""},
		"resumed after a pause":             {[]provider.State{provider.StateRunning, provider.StatePaused, provider.StateStarting, provider.StateRunning}, ""},
		"starting under a command that ran": {[]provider.State{provider.StateStarting, provider.StateRunning, provider.StateStarting}, "starting after its command ran"},
		"starting after it exited":          {[]provider.State{provider.StateExited, provider.StateStarting}, "starting after its command ran"},
		"a state of the vendor's own":       {[]provider.State{"hibernating"}, `state "hibernating"`},
		"no state":                          {[]provider.State{""}, `state ""`},
	} {
		t.Run(name, func(t *testing.T) {
			r := &run{ran: map[string]bool{}}
			held := &faulted{TB: t}
			for _, state := range tc.states {
				r.checkState(held, provider.Instance{ID: "1", Name: "hlconf-a", State: state})
			}
			switch {
			case tc.fault == "" && len(held.faults) != 0:
				t.Fatalf("faults = %q, want none", held.faults)
			case tc.fault != "" && (len(held.faults) != 1 || !strings.Contains(held.faults[0], tc.fault)):
				t.Fatalf("faults = %q, want one saying %q", held.faults, tc.fault)
			}
		})
	}
}
