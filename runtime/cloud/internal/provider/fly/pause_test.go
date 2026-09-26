package fly

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const (
	suspendPath = machinesPath + "/9001/suspend"
	startPath   = machinesPath + "/9001/start"
)

func TestPauseSuspendsTheMachineAndNeverStopsIt(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, suspendPath, answer(200, `{"ok":true}`))
	p := newTestProvider(t, api)

	if err := p.Pause(context.Background(), "9001"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if sent := api.sent(http.MethodPost, suspendPath); len(sent) != 1 || api.total() != 1 || sent[0].Auth != testToken {
		t.Fatalf("requests = %d, of them suspends = %+v: a stop would boot the image's filesystem again", api.total(), sent)
	}
}

func TestPauseOfAMachineThatIsGoneIsNotAnErrorAndARefusalIs(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, suspendPath, inSequence(
		answer(404, `{"error":"machine not found"}`),
		answer(422, `{"error":"machines with more than 2048 MB of memory cannot be suspended"}`),
	))
	p := newTestProvider(t, api)

	if err := p.Pause(context.Background(), "9001"); err != nil {
		t.Fatalf("Pause of a machine that is gone: %v", err)
	}
	if err := p.Pause(context.Background(), "9001"); err == nil || !strings.Contains(err.Error(), "cannot be suspended") {
		t.Fatalf("Pause the vendor refused: err = %v, want the vendor's words", err)
	}
}

func TestPauseRefusedForAMachineBeingDestroyedIsNotAnErrorAndForALivingOneIs(t *testing.T) {
	refusal := answer(409, `{"error":"aborted: unable to suspend machine, current state invalid, destroyed"}`)
	for state, wantErr := range map[string]bool{"destroyed": false, "destroying": false, "stopped": true} {
		api := newFakeAPI(t)
		api.on(http.MethodPost, suspendPath, refusal)
		api.on(http.MethodGet, machinesPath+"/9001", answer(200, machineJSON("9001", "hl-a", state, 0)))
		err := newTestProvider(t, api).Pause(context.Background(), "9001")
		if (err != nil) != wantErr {
			t.Errorf("Pause refused for a %s machine: err = %v", state, err)
		}
	}
}

func TestResumeStartsTheMachineAndSaysWhenItIsGone(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, startPath, inSequence(answer(200, `{"previous_state":"suspended"}`), answer(404, `{"error":"machine not found"}`)))
	p := newTestProvider(t, api)

	if err := p.Resume(context.Background(), "9001"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := p.Resume(context.Background(), "9001"); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("Resume of a machine that is gone: err = %v", err)
	}
}
