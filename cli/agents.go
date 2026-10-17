package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

var agentsCommands = []subcommand{
	{"add", "create one agent in a platform experiment, or join a registered one with --agent", agentsAdd},
	{"remove", "take an agent out of a platform experiment that has not started", agentsRemove},
	{"list", "the agents of a platform experiment and what each was launched with", agentsList},
	{"finish", "say that you, an agent, have nothing left to do: your machine stops", agentsFinish},
	{"message", "tell an agent, with --all every researcher or with --coordinator the coordinator, a direction directly: it is woken with it", agentsMessage},
	{"messages", "the messages sent to agents, oldest first; an agent's token reads its own", agentsMessages},
	{"session", "the stored model session of an agent, the coordinator or the planner", agentsSession},
}

// rolesOf is the role of each agent that joined the platform experiment, by agent id.
func rolesOf(c *apiclient.Client, platformExperiment string) (map[string]platformapi.SignupRole, error) {
	var roster struct {
		Agents []struct {
			ID   string                 `json:"agent_id"`
			Role platformapi.SignupRole `json:"role"`
		} `json:"agents"`
	}
	if err := c.Do("GET", apiclient.AgentsPath(platformExperiment), nil, &roster); err != nil {
		return nil, err
	}
	roles := make(map[string]platformapi.SignupRole, len(roster.Agents))
	for _, signup := range roster.Agents {
		roles[signup.ID] = signup.Role
	}
	return roles, nil
}

func agentsAdd(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents add", `usage: hl platform-experiments agents add [--platform-experiment PE] [--hosted] [--agent-type TYPE] [--hyperparameters JSON] [--model M] [--name NAME] [--role ROLE] [--quota-tier TIER]
   or: hl platform-experiments agents add [--platform-experiment PE] --agent ID [--role ROLE] [--quota-tier TIER]

The only way an agent joins a platform experiment, while it is open; once per agent. While it
runs, the coordinator or the owner adds a researcher in a place max_agents leaves free, with a
share of what the researchers left unspent; the platform itself replaces a researcher of the
experiment's researchers that finished or lost its machine. The first form creates one; the
second joins a registered one again under its id (a removed agent), or prints a self-run agent's
token again. max_agents counts the researchers (role competitor) at work. A reviewer re-checks others' claims and records agreement or dispute; like a
baseline agent it is not ranked, cut or counted.
--hosted: the platform runs the agent, starting a baseline agent's machine at once and any
other's at the start of the run (its status_reason says what the start waits for). Without --hosted the line printed
carries the agent's token: run the agent with it as HL_TOKEN and the API's address as API_URL,
from an env file the container reads, never a command line.`, stderr)
	agent := fs.String("agent", "", "join this registered agent instead of creating a new one")
	hosted := fs.Bool("hosted", false, "the platform runs the agent")
	agentType := fs.String("agent-type", "", "the agent type the agent runs, by id; omitted, generalist")
	hyperparameters := fs.String("hyperparameters", "", "the direction you give the agent, a JSON object of any keys: the approach it explores")
	model := fs.String("model", "", "the model the agent runs on; omitted, its agent type's")
	name := fs.String("name", "", "display name of a created agent, unique in the platform experiment; omitted, its role and a number")
	role := fs.String("role", "", "competitor (default), baseline or reviewer")
	quotaTier := fs.String("quota-tier", "", "guaranteed or burst_only; omit to use the platform experiment's default_quota_tier")
	named := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	pe, err := command.NamedExperiment(fs, c, *named)
	if err != nil {
		return command.ParseExit(err)
	}
	if !experimentspec.ValidQuotaTierOverride(*quotaTier) {
		return command.ParseExit(command.Misuse(fs, "--quota-tier must be %q or %q, got %s", experimentspec.QuotaTierGuaranteed, experimentspec.QuotaTierBurstOnly, *quotaTier))
	}
	body := map[string]any{}
	for key, value := range map[string]string{
		"agent_id": *agent, "agent_type": *agentType, "model": *model, "name": *name, "role": *role, "quota_tier": *quotaTier,
	} {
		if value != "" {
			body[key] = value
		}
	}
	if *hosted {
		body["hosted"] = true
	}
	if *hyperparameters != "" {
		var object map[string]any
		if err := json.Unmarshal([]byte(*hyperparameters), &object); err != nil || object == nil {
			return command.ParseExit(command.Misuse(fs, "--hyperparameters must be a JSON object, got %s", *hyperparameters))
		}
		body["hyperparameters"] = object
	}
	return command.Call(c, "POST", apiclient.AgentsPath(pe), body, "agents", "no agent was added", stdout, stderr)
}

func agentsRemove(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents remove", `usage: hl platform-experiments agents remove [--platform-experiment PE] AGENT

Takes the agent out of a platform experiment that is still open: its token stops working and a
hosted agent's machine is taken down. A coordinator's token removes only an agent it added,
and 403 agent_not_added_by_coordinator answers for any other.
Refused once the experiment has started; from then on an agent leaves by being cut or by the
experiment closing. Refused with 409 work_unfinished while the agent has a job queued or
running, which a baseline agent may: cancel the job or let it end first.`, stderr)
	named := command.ExperimentFlag(fs)
	agent, err := command.ParseOne(fs, args, "AGENT", "", "agent")
	if err != nil {
		return command.ParseExit(err)
	}
	pe, err := command.NamedExperiment(fs, c, *named)
	if err != nil {
		return command.ParseExit(err)
	}
	return command.Call(c, "DELETE", apiclient.AgentsPath(pe)+"/"+url.PathEscape(agent), nil, "", fmt.Sprintf("removed %s from platform experiment %s", agent, pe), stdout, stderr)
}

func agentsList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents list", `usage: hl platform-experiments agents list [--platform-experiment PE]

Every agent that joined the platform experiment, in the order they did, each with its role and
in actor whose credential added it. A draft's planner has role planner. While the platform
experiment is open, every agent but a baseline agent waits for the start and says so in
status_reason, with the reason the experiment is not running yet: no machine is asked for it
before, and its instance.state reads waiting until then. After the start instance.state is
requested once a machine is asked for, starting while it boots, running, and stopped.
in_flight counts its jobs and checks queued or running: both 0 is nothing of its own in flight.
No token is listed:
hl platform-experiments agents add --agent ID prints a self-run agent's token again.`, stderr)
	named := command.ExperimentFlag(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	pe, err := command.NamedExperiment(fs, c, *named)
	if err != nil {
		return command.ParseExit(err)
	}
	return command.Call(c, "GET", apiclient.AgentsPath(pe), nil, "agents", fmt.Sprintf("no agents have joined %s", pe), stdout, stderr)
}

func messagesPath(platformExperiment string) string {
	return "/platform-experiments/" + url.PathEscape(platformExperiment) + "/messages"
}

func agentsMessage(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents message", `usage: hl platform-experiments agents message [--platform-experiment PE] AGENT --text T
   or: hl platform-experiments agents message [--platform-experiment PE] --all --text T
   or: hl platform-experiments agents message [--platform-experiment PE] --coordinator --text T

Tells the agent, or every researcher at work, something directly: it is woken at once with
"message from the coordinator: T" (or the owner), as agent.message, and is told it again after a
restart until it was delivered. Use it to steer; a fact about a hypothesis goes in a comment.
--coordinator tells the hosted coordinator, from a person or a key, the same way (409
no_coordinator where none is hosted). At most 1500 bytes. A recipient is sent at most 6 messages
an hour by the coordinator and 6 by the owner, counted apart: past that it is refused (429
message_rate_limited) with when the next can go. With --all, researchers at their limit are
skipped and the rest are sent: one line per message sent, then one line per researcher refused
(agent_id, reason, retry_after) and a note on stderr; refused whole only when none could be sent.
Refused for an agent that finished or was cut (409 agent_ended). Prints one line per message
sent. hl platform-experiments agents messages reads what was sent before. --text-file FILE, or - for
stdin, gives T in place of --text T.`, stderr)
	named := command.ExperimentFlag(fs)
	message := command.NewTextFlag(fs, "text", "what the agent is told")
	all := fs.Bool("all", false, "every researcher at work")
	coordinator := fs.Bool("coordinator", false, "the hosted coordinator")
	operands, err := command.ParseOperands(fs, args, "agent")
	if err != nil {
		return command.ParseExit(err)
	}
	text, err := message.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	switch {
	case *all && *coordinator, (*all || *coordinator) && len(operands) > 0, !*all && !*coordinator && len(operands) != 1:
		return command.ParseExit(command.Misuse(fs, "want one AGENT, --all or --coordinator"))
	case strings.TrimSpace(text) == "":
		return command.ParseExit(command.Misuse(fs, "want --text or --text-file"))
	}
	pe, err := command.NamedExperiment(fs, c, *named)
	if err != nil {
		return command.ParseExit(err)
	}
	body := map[string]any{"text": text}
	switch {
	case *all:
		body["all"] = true
	case *coordinator:
		body["coordinator"] = true
	default:
		body["agent_id"] = operands[0]
	}
	var out struct {
		Messages json.RawMessage   `json:"messages"`
		Refused  []json.RawMessage `json:"refused"`
	}
	if err := c.Do("POST", messagesPath(pe), body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLines(stdout, stderr, "no message was sent", out.Messages); err != nil {
		return command.Fail(stderr, err)
	}
	for _, refused := range out.Refused {
		if err := command.PrintLine(stdout, refused); err != nil {
			return command.Fail(stderr, err)
		}
	}
	if len(out.Refused) > 0 {
		fmt.Fprintf(stderr, "hl: %d recipients were not sent to: rate limited, see refused above\n", len(out.Refused))
	}
	return 0
}

func agentsMessages(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents messages", `usage: hl platform-experiments agents messages [--platform-experiment PE] [--agent ID]

Every message the coordinator and the owner sent to an agent of the platform experiment, and the
owner to its coordinator, oldest first, one per line: agent_id (coordinator for the
coordinator's), from (coordinator or owner), text, actor and at. --agent narrows it to one
agent's, --agent coordinator to the coordinator's. An agent's token reads its own alone.`, stderr)
	named := command.ExperimentFlag(fs)
	agent := fs.String("agent", "", "only the messages to this agent")
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	pe, err := command.NamedExperiment(fs, c, *named)
	if err != nil {
		return command.ParseExit(err)
	}
	path := messagesPath(pe)
	if *agent != "" {
		path += "?agent_id=" + url.QueryEscape(*agent)
	}
	return command.Call(c, "GET", path, nil, "messages", "no message was sent", stdout, stderr)
}

func agentsFinish(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("platform-experiments agents finish", `usage: hl platform-experiments agents finish --reason TEXT [--blocker TEXT] [--baseline JOB]

--reason-file and --blocker-file read the text from a FILE, or stdin for -, in place of the
inline flag.

Says that you have nothing left to do: your quota is spent, or nothing you could still try is
worth a job. Your machine stops after this turn and the coordinator is told the reason, with the
time and quota you leave. It cannot be taken back. A researcher with more than a tenth of the
run's time and of its quota left is refused (time_and_quota_left), told how much of each is
left, unless --blocker says what stops it that the time and quota cannot fix. Refused while you have a job queued or running or a completed job without
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
	blockerFlag := command.NewTextFlag(fs, "blocker", "what stops you that the time and quota left cannot fix")
	baseline := fs.String("baseline", "", "an agent of role baseline: the job of the control to declare as the baseline")
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	reason, err := reasonFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	blocker, err := blockerFlag.Value(fs)
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
	if blocker != "" {
		body["blocker"] = blocker
	}
	if *baseline != "" {
		body["baseline_job_id"] = *baseline
	}
	if err := c.Do("POST", apiclient.AgentsPath(pe)+"/"+url.PathEscape(agent)+"/finish", body, &out); err != nil {
		if reason, _ := command.RefusalOf(err); reason == "experiment_closed" {
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
