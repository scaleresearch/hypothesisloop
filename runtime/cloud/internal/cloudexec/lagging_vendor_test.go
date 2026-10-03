package cloudexec

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/exitline"
)

// laggingVendor lists every instance in the state it was in `behind` lists ago, as a vendor does
// whose hosts report to it on their own schedule. Output is read from the machine and does not
// lag.
type laggingVendor struct {
	*fake.Provider
	behind int
	states []map[string]provider.State
}

func (l *laggingVendor) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	listed, err := l.Provider.List(ctx, namePrefix)
	if err != nil {
		return nil, err
	}
	now := map[string]provider.State{}
	for _, i := range listed {
		now[i.ID] = i.State
	}
	l.states = append(l.states, now)
	var then map[string]provider.State
	if at := len(l.states) - 1 - l.behind; at >= 0 {
		then = l.states[at]
	}
	for n := range listed {
		listed[n].State = provider.StateStarting
		if state, existed := then[listed[n].ID]; existed {
			listed[n].State = state
		}
	}
	return listed, nil
}

// What every adapter's vendor may do to a short job: the command starts and ends between two
// reads, or while the vendor still says the machine is starting. The job is then never seen
// running, and when it ran is still what the wrapper measured, to the millisecond, whenever the
// vendor gets round to saying it exited.
func TestARunIsTimedByTheWrapperHoweverLateTheVendorReportsIt(t *testing.T) {
	for name, behind := range map[string]int{"a vendor in step": 0, "a vendor two reads behind": 2} {
		t.Run(name, func(t *testing.T) {
			vendor := &laggingVendor{Provider: fake.New(), behind: behind}
			e, err := New(testConfig(vendor))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			const instance = "hl-job-t1-exp-1-a0-cpu-0"
			mustCreate(t, e, job(t, "exp-1", 0, 0))

			started := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
			ended := started.Add(12345 * time.Millisecond)
			vendor.Start(instance)
			vendor.Exit(instance, "epoch 1", exitline.FormatRun(started, ended), exitline.Format(0))

			for read := 0; read < behind; read++ {
				observed, err := e.PollJobStatus(ctx, "exp-1")
				if err != nil || observed.Phase != workload.JobPhasePending || !observed.StartedAt.IsZero() || !observed.EndedAt.IsZero() {
					t.Fatalf("read %d behind the vendor: %+v, %v, want a pending job with no run", read, observed, err)
				}
			}
			observed, err := e.PollJobStatus(ctx, "exp-1")
			if err != nil || observed.Phase != workload.JobPhaseSucceeded || !observed.StartedAt.Equal(started) || !observed.EndedAt.Equal(ended) {
				t.Fatalf("once the vendor says it exited: %+v, %v, want succeeded and the run %v to %v", observed, err, started, ended)
			}
		})
	}
}

func TestARunIsReportedOnlyWithTheExitLineThatEndsIt(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	run := exitline.FormatRun(started, started.Add(time.Second))
	for name, tc := range map[string]struct {
		lines []string
		phase workload.JobPhase
		timed bool
	}{
		"failed":                       {[]string{run, exitline.Format(3)}, workload.JobPhaseFailed, true},
		"a wrapper that timed nothing": {[]string{exitline.Format(0)}, workload.JobPhaseSucceeded, false},
		"a run line the job printed":   {[]string{run, "Killed"}, workload.JobPhaseGone, false},
	} {
		e, p := newTestExecutor(t)
		mustCreate(t, e, job(t, "exp-1", 0, 0))
		p.Exit("hl-job-t1-exp-1-a0-cpu-0", tc.lines...)
		observed, err := e.PollJobStatus(context.Background(), "exp-1")
		if err != nil || observed.Phase != tc.phase || observed.EndedAt.IsZero() == tc.timed || observed.StartedAt.IsZero() == tc.timed {
			t.Errorf("%s: %+v, %v", name, observed, err)
		}
	}
}
