package launcherapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The declarations below are the launcher API as the control plane and the launcher each
// declared it before it had a contract. Launchers and control planes of either age read what
// the other sends during a rollout.

type oldServerDesired struct {
	Instances []oldServerInstance `json:"instances"`
	Ending    []string            `json:"ending"`
	Image     string              `json:"image,omitempty"`
	Refusal   *oldServerRefusal   `json:"refusal,omitempty"`
}

type oldServerInstance struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env"`
}

type oldServerRefusal struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type oldLauncherAnswer struct {
	Instances *[]struct {
		Name  string            `json:"name"`
		Image string            `json:"image"`
		Env   map[string]string `json:"env"`
	} `json:"instances"`
	Ending  *[]string `json:"ending"`
	Image   string    `json:"image"`
	Refusal *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"refusal"`
}

type oldObservedInstance struct {
	Name         string  `json:"name"`
	Machine      string  `json:"machine"`
	State        string  `json:"state"`
	Log          string  `json:"log,omitempty"`
	Expired      bool    `json:"expired,omitempty"`
	Progress     *string `json:"progress,omitempty"`
	Activity     string  `json:"activity,omitempty"`
	HarnessState string  `json:"harness_state,omitempty"`
}

type oldQuestion struct {
	LaunchDigest             string                `json:"launch_digest"`
	Instances                []oldObservedInstance `json:"instances"`
	ReconcileIntervalSeconds int                   `json:"reconcile_interval_seconds"`
	CreatePauseMilliseconds  int                   `json:"create_pause_milliseconds"`
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	wire, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestTheDesiredStateIsTheOneLaunchersRead(t *testing.T) {
	for name, c := range map[string]struct {
		was oldServerDesired
		now Desired
	}{
		"instances and ending": {
			was: oldServerDesired{
				Instances: []oldServerInstance{{Name: "pe-ag-0123", Image: "registry.test/agent:1", Env: map[string]string{"API_URL": "https://cp.test", "HL_TOKEN": "hls_x"}}},
				Ending:    []string{"pe-ag-old"}, Image: "registry.test/agent:1",
			},
			now: Desired{
				Instances: []Instance{{Name: "pe-ag-0123", Image: "registry.test/agent:1", Env: map[string]string{"API_URL": "https://cp.test", "HL_TOKEN": "hls_x"}}},
				Ending:    []string{"pe-ag-old"}, Image: "registry.test/agent:1",
			},
		},
		"refused with empty lists": {
			was: oldServerDesired{Instances: []oldServerInstance{}, Ending: []string{}, Refusal: &oldServerRefusal{Reason: RefusalPassDoesNotFit, Message: "lower create_pause"}},
			now: Desired{Instances: []Instance{}, Ending: []string{}, Refusal: &Refusal{Reason: RefusalPassDoesNotFit, Message: "lower create_pause"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			was, now := encode(t, c.was), encode(t, c.now)
			if string(was) != string(now) {
				t.Fatalf("encodes as %s\nwas %s", now, was)
			}
			var oldRead oldLauncherAnswer
			if err := json.Unmarshal(now, &oldRead); err != nil || oldRead.Instances == nil || oldRead.Ending == nil {
				t.Fatalf("a launcher built before the contract reads %+v, %v: want both lists", oldRead, err)
			}
			var read Desired
			if err := json.Unmarshal(was, &read); err != nil || !reflect.DeepEqual(read, c.now) {
				t.Fatalf("the old answer reads as %+v, %v", read, err)
			}
		})
	}

	// An answer without a list reads as one without it, which a launcher refuses whole.
	var read Desired
	if err := json.Unmarshal([]byte(`{"instances":null}`), &read); err != nil || read.Instances != nil || read.Ending != nil {
		t.Fatalf("an answer without lists reads as %+v, %v", read, err)
	}
}

func TestTheObservedStateIsTheOneLaunchersSent(t *testing.T) {
	progress := "step 3"
	was := oldQuestion{LaunchDigest: "0123456789abcdef", ReconcileIntervalSeconds: 30, CreatePauseMilliseconds: 500, Instances: []oldObservedInstance{
		{Name: "a", Machine: "m-1", State: StateExited, Log: "bye", Expired: true},
		{Name: "b", Machine: "m-2", State: StateRunning, Progress: &progress, Activity: "reading", HarnessState: "subagents=2 turn=working"},
		{Name: "c", Machine: "m-3", State: StateStarting},
	}}
	now := Observed{LaunchDigest: "0123456789abcdef", ReconcileIntervalSeconds: 30, CreatePauseMilliseconds: 500, Instances: []ObservedInstance{
		{Name: "a", Machine: "m-1", State: StateExited, Log: "bye", Expired: true},
		{Name: "b", Machine: "m-2", State: StateRunning, Progress: &progress, Activity: "reading", HarnessState: "subagents=2 turn=working"},
		{Name: "c", Machine: "m-3", State: StateStarting},
	}}
	if was, now := encode(t, was), encode(t, now); string(was) != string(now) {
		t.Fatalf("encodes as %s\nwas %s", now, was)
	}
}

func TestTheHeldMachinesAreTheOnesLaunchersRead(t *testing.T) {
	type oldHeld struct {
		Machines *[]struct {
			Name    string `json:"name"`
			Machine string `json:"machine"`
		} `json:"machines"`
	}
	wire := encode(t, Held{Machines: []HeldMachine{{Name: "a", Machine: "m-1"}}})
	if string(wire) != `{"machines":[{"name":"a","machine":"m-1"}]}` {
		t.Fatalf("encodes as %s", wire)
	}
	var read oldHeld
	if err := json.Unmarshal(encode(t, Held{Machines: []HeldMachine{}}), &read); err != nil || read.Machines == nil {
		t.Fatalf("an empty list reads as %+v, %v: want a list", read, err)
	}
}
