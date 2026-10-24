package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func AgentsFinish(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents finish", `usage: hl platform-experiments agents finish --reason TEXT [--because blocked|nothing_left] [--baseline JOB]

--reason-file reads the reason from a FILE, or stdin for -, in place of --reason.

Says that you have nothing left to do: your quota is spent, something stops you that the time
and quota left cannot fix, or nothing you could still try is worth a job. Your machine stops
after this turn and the coordinator is told the reason, with the time and quota you leave. It
cannot be taken back. An executor with more than a tenth of the run's time and of its quota
left, and more time than its jobs take, says why it stops: --because blocked, the reason naming
what stops it, or --because nothing_left, the reason saying what you tried and why no idea left
is worth a job. Without it such a finish is refused (time_and_quota_left), told how much of
each is left. Refused while you have a job queued or running or a completed job without
its summary (work_unfinished). Once the platform experiment closed there is nothing to finish:
hl says so and exits 0. Prints you as finished. Your token says who and where. Each hypothesis
of yours still open is listed after you and stays open for good unless you give its verdict
(hl hypothesis status ID VERDICT) before this turn ends.

An agent of role baseline names with --baseline the COMPLETED job of the control, which the
platform declares as the baseline in this request. It must be your own job and pass every check
of a declared baseline, or the finish is refused with the reason (422) and nothing is recorded:
fix what it says and finish again. Without --baseline the platform declares a job only when the
baseline agents left exactly one that reports the ranking metric.`, stderr)
	reasonFlag := command.NewTextFlag(fs, "reason", "why nothing is left to do")
	because := fs.String("because", "", "why you stop with time and quota left: blocked or nothing_left")
	baseline := fs.String("baseline", "", "an agent of role baseline: the job of the control to declare as the baseline")
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	reason, err := reasonFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if strings.TrimSpace(reason) == "" {
		return command.ParseExit(command.Misuse(fs, "want --reason or --reason-file"))
	}
	_, pe, agent := apiclient.ScopeOf(c.Token)
	if agent == "" {
		return command.Fail(stderr, errors.New("platform-experiments agents finish: an agent finishes itself, with its own token, and HL_TOKEN names no agent"))
	}
	var out struct {
		Agents []json.RawMessage `json:"agents"`
		Open   []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"open_hypotheses"`
	}
	body := map[string]string{"reason": reason}
	if *because != "" {
		body["because"] = *because
	}
	if *baseline != "" {
		body["baseline_job_id"] = *baseline
	}
	if err := c.Do("POST", apiclient.AgentsPath(pe)+"/"+url.PathEscape(agent)+"/finish", body, &out); err != nil {
		if reason, _ := command.RefusalOf(err); reason == platformapi.ReasonExperimentClosed {
			fmt.Fprintln(stderr, "hl: platform-experiments agents finish: the platform experiment is closed, which ended your run: nothing needs finishing, so end your turn")
			return 0
		}
		return command.Fail(stderr, err)
	}
	for _, finished := range out.Agents {
		if err := command.PrintLine(stdout, finished); err != nil {
			return command.Fail(stderr, err)
		}
	}
	if len(out.Open) > 0 {
		fmt.Fprintf(stderr, "hl: platform-experiments agents finish: note: you finished with %d hypotheses still open whose verdict is yours, your own and the ideas of people your jobs tested. Each stays open for good unless you give its verdict before this turn ends, with hl hypothesis status ID VERDICT:\n", len(out.Open))
		for _, h := range out.Open {
			fmt.Fprintf(stderr, "  %s %s\n", h.ID, strings.Join(strings.Fields(h.Text), " "))
		}
	}
	return 0
}
