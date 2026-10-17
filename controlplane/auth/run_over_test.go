package auth

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A token that still works after its run ended says so, for whatever must not outlive the
// wind-down. A job says it for its platform experiment and for the agent it belongs to.
func TestATokenWhoseRunEndedSaysSo(t *testing.T) {
	ctx := context.Background()
	inWindDown := testNow.Add(-domain.AgentWindDown + time.Second)
	job := func(d *fakeDirectory) {
		owned := tenancyA
		owned.AgentID = "agent-1"
		d.jobs["job-a"] = owned
	}
	for name, tc := range map[string]struct {
		scope  string
		change func(*fakeDirectory)
		want   bool
	}{
		"agent at work":                           {scope.Agent("pe-a", "agent-1"), func(*fakeDirectory) {}, false},
		"agent after the close":                   {scope.Agent("pe-a", "agent-1"), func(d *fakeDirectory) { d.closedAt["pe-a"] = inWindDown }, true},
		"agent after its cut":                     {scope.Agent("pe-a", "agent-1"), func(d *fakeDirectory) { d.cutAt["pe-a/agent-1"] = inWindDown }, true},
		"agent whose rival was cut":               {scope.Agent("pe-a", "agent-1"), func(d *fakeDirectory) { d.cutAt["pe-a/agent-2"] = inWindDown }, false},
		"planner whose answer is stored":          {scope.Agent("pe-a", "agent-1"), func(d *fakeDirectory) { d.answeredAt["pe-a/agent-1"] = inWindDown }, true},
		"agent that finished":                     {scope.Agent("pe-a", "agent-1"), func(d *fakeDirectory) { d.finishedAt["pe-a/agent-1"] = inWindDown }, true},
		"agent whose rival finished":              {scope.Agent("pe-a", "agent-1"), func(d *fakeDirectory) { d.finishedAt["pe-a/agent-2"] = inWindDown }, false},
		"coordinator at work":                     {scope.Coordinator("pe-a"), func(*fakeDirectory) {}, false},
		"coordinator after the close":             {scope.Coordinator("pe-a"), func(d *fakeDirectory) { d.closedAt["pe-a"] = inWindDown }, true},
		"job at work":                             {scope.Job("job-a"), job, false},
		"job after the close":                     {scope.Job("job-a"), func(d *fakeDirectory) { job(d); d.closedAt["pe-a"] = testNow }, true},
		"job long after the close, still running": {scope.Job("job-a"), func(d *fakeDirectory) { job(d); d.closedAt["pe-a"] = testNow.Add(-time.Hour) }, true},
		"job whose agent was cut":                 {scope.Job("job-a"), func(d *fakeDirectory) { job(d); d.cutAt["pe-a/agent-1"] = inWindDown }, true},
		"job of an agent whose rival was cut":     {scope.Job("job-a"), func(d *fakeDirectory) { job(d); d.cutAt["pe-a/agent-2"] = inWindDown }, false},
	} {
		a, directory := newTestAuthenticator(t)
		tc.change(directory)
		p, err := a.Authenticate(ctx, mustMint(t, testSecret, tc.scope))
		if err != nil || p.RunOver != tc.want {
			t.Errorf("%s: got = %+v, %v, want RunOver = %v", name, p, err, tc.want)
		}
	}
}
