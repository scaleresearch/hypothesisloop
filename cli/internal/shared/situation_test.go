package shared

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

const situationRecord = `{
  "run": {"now": "2026-10-09T14:00:00.123456Z", "ends_at": "2026-10-09T20:00:00Z", "minutes_left": 360,
          "stage": {"index": 1, "name": "explore", "next_boundary_at": "2026-10-09T16:00:00Z", "next_share_acch": 12.5}},
  "objective": [{"metric": "val_loss", "direction": "minimize", "baseline_value": 2.3123456789, "best_value": 2.1,
                 "best_job": "job-7", "best_agent": "ag-1", "target": 1.9, "gap_to_target_pct": 10.526315789,
                 "best_1h_ago": 2.2, "best_3h_ago": null}],
  "capacity": {"clusters": [{"cluster": "gpu-east", "resource": "H100", "free": 4, "total": 8}],
               "fits_now": 4, "jobs_in_flight_run": {"queued": 0, "running": 4}},
  "budget": {"run_budget_acch": 100, "run_used_acch": 40.25},
  "agents": [{"agent_id": "ag-1", "name": "Executor 1", "role": "competitor", "jobs": {"queued": 0, "running": 2},
              "subagents": 3, "turn": {"state": "working", "since": "2026-10-09T13:55:00Z"},
              "activity": {"text": "reading job-7 logs", "at": "2026-10-09T13:59:00Z"},
              "minutes_without_job_in_flight": 0, "budget": {"share": 20, "used": 8, "room_guaranteed": 10, "room_burst": 12},
              "messages": {"left_this_hour": 5, "next_at": null}, "model": {"window": "5h", "utilization": 0.41, "refused_until": null}}],
  "coordinator": {"summary_written_at": "2026-10-09T12:00:00Z", "model": {"window": "5h", "utilization": 0.2}}
}`

// An agent reads the situation in every prompt: one line per block, a list one line per element,
// what has no value left out, so the whole run reads at a glance.
func TestSituationPrintsOneLinePerBlock(t *testing.T) {
	var asked string
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		_, _ = io.WriteString(w, situationRecord)
	}))
	t.Setenv("HL_TOKEN", clitest.DerivedToken("agent:pe-1/ag-1"))
	code, stdout, stderr := clitest.Output(t, Situation)
	if code != 0 || stderr != "" || asked != "GET /platform-experiments/pe-1/situation" {
		t.Fatalf("exit %d, asked %q, stderr %q", code, asked, stderr)
	}
	want := strings.Join([]string{
		"run: now=2026-10-09T14:00:00Z ends_at=2026-10-09T20:00:00Z minutes_left=360 stage.index=1 stage.name=explore stage.next_boundary_at=2026-10-09T16:00:00Z stage.next_share_acch=12.5",
		"objective: metric=val_loss direction=minimize baseline_value=2.31235 best_value=2.1 best_job=job-7 best_agent=ag-1 target=1.9 gap_to_target_pct=10.5263 best_1h_ago=2.2",
		"capacity: fits_now=4 jobs_in_flight_run.queued=0 jobs_in_flight_run.running=4",
		"capacity.clusters: cluster=gpu-east resource=H100 free=4 total=8",
		"budget: run_budget_acch=100 run_used_acch=40.25",
		`agents: agent_id=ag-1 name="Executor 1" role=competitor jobs.queued=0 jobs.running=2 subagents=3 turn.state=working turn.since=2026-10-09T13:55:00Z activity.text="reading job-7 logs" activity.at=2026-10-09T13:59:00Z minutes_without_job_in_flight=0 budget.share=20 budget.used=8 budget.room_guaranteed=10 budget.room_burst=12 messages.left_this_hour=5 model.window=5h model.utilization=0.41`,
		"coordinator: summary_written_at=2026-10-09T12:00:00Z model.window=5h model.utilization=0.2",
	}, "\n") + "\n"
	if stdout != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", stdout, want)
	}

	code, stdout, _ = clitest.Output(t, Situation, "--json")
	if code != 0 || stdout != situationRecord+"\n" {
		t.Errorf("--json: exit %d, stdout %s, want the record as the platform answered it", code, stdout)
	}
}

func TestSituationSaysAnEmptyListIsEmpty(t *testing.T) {
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"run":{"minutes_left":5},"agents":[]}`)
	}))
	code, stdout, stderr := clitest.Output(t, Situation, "--platform-experiment", "pe-1")
	if code != 0 || stdout != "run: minutes_left=5\nagents: none\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// A closed run reads no minutes left and when and why it closed, and one not started says so.
func TestSituationStatesAClosedOrNotStartedRun(t *testing.T) {
	for record, want := range map[string]string{
		`{"run":{"now":"2026-10-09T14:00:00Z","status":"closed","closed_at":"2026-10-09T13:00:00Z","close_reason":"closed_by_request","minutes_left":0}}`: "run: now=2026-10-09T14:00:00Z status=closed closed_at=2026-10-09T13:00:00Z close_reason=closed_by_request minutes_left=0\n",
		`{"run":{"now":"2026-10-09T14:00:00Z","status":"open","not_started":true,"duration_hours":4}}`:                                                    "run: now=2026-10-09T14:00:00Z status=open not_started=true duration_hours=4\n",
	} {
		clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, record) }))
		code, stdout, stderr := clitest.Output(t, Situation, "--platform-experiment", "pe-1")
		if code != 0 || stdout != want {
			t.Errorf("exit %d, stdout %q, want %q, stderr %q", code, stdout, want, stderr)
		}
	}
}
