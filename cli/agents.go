package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

var agentsCommands = []subcommand{
	{"add", "create one agent in a platform experiment, or join a registered one with --agent", agentsAdd},
	{"remove", "take an agent out of a platform experiment that has not started", agentsRemove},
	{"list", "the agents of a platform experiment and what each was launched with", agentsList},
	{"finish", "say that you, an agent, have nothing left to do: your machine stops", agentsFinish},
	{"session", "the stored model session of an agent, the coordinator or the planner", agentsSession},
}

func agentsPath(platformExperiment string) string {
	return "/platform-experiments/" + url.PathEscape(platformExperiment) + "/agents"
}

// signupPath is where an agent's own signup is read, by the agent itself too: the roster above
// is the coordinator's.
func signupPath(platformExperiment, agent string) string {
	return "/platform-experiments/" + url.PathEscape(platformExperiment) + "/signups/" + url.PathEscape(agent)
}

// rolesOf is the role of each agent that joined the platform experiment, by agent id.
func rolesOf(c *apiclient.Client, platformExperiment string) (map[string]string, error) {
	var roster struct {
		Agents []struct {
			ID   string `json:"agent_id"`
			Role string `json:"role"`
		} `json:"agents"`
	}
	if err := c.Do("GET", agentsPath(platformExperiment), nil, &roster); err != nil {
		return nil, err
	}
	roles := make(map[string]string, len(roster.Agents))
	for _, signup := range roster.Agents {
		roles[signup.ID] = signup.Role
	}
	return roles, nil
}

func agentsAdd(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments agents add", `usage: hl platform-experiments agents add [--platform-experiment PE] [--hosted] [--flavor F] [--hyperparameters JSON] [--model M] [--name NAME] [--role ROLE] [--quota-tier TIER]
   or: hl platform-experiments agents add [--platform-experiment PE] --agent ID [--role ROLE] [--quota-tier TIER]

The only way an agent joins a platform experiment, and only while it is open. The first form creates one agent; the second joins one that is
registered already. For several agents, run it once for each. With --hosted the platform runs the agent:
a baseline agent's machine at once, any other's from the start of the run, and until then its
status_reason says what the start waits for.
max_agents counts researchers (role competitor) alone, finished ones too.
Without it the line printed carries the agent's token: start the agent yourself with that as
HL_TOKEN and the API's address as API_URL, which is all it needs. Keep the token out of
command lines and shell history; write it to an env file the container reads.`, stderr)
	agent := fs.String("agent", "", "join this registered agent instead of creating a new one")
	hosted := fs.Bool("hosted", false, "the platform runs the agent")
	flavor := fs.String("flavor", "", "the specialization the agent runs")
	hyperparameters := fs.String("hyperparameters", "", "a JSON object of knobs the agent reads back")
	model := fs.String("model", "", "the model the agent runs on; omitted, the platform's default")
	name := fs.String("name", "", "display name of a created agent, unique in the platform experiment; omitted, its role and a number")
	role := fs.String("role", "", "competitor (default), baseline or reviewer")
	quotaTier := fs.String("quota-tier", "", "guaranteed or burst_only; omit to use the platform experiment's default_quota_tier")
	named := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	pe, err := namedExperiment(fs, c, *named)
	if err != nil {
		return parseExit(err)
	}
	if *quotaTier != "" && *quotaTier != "guaranteed" && *quotaTier != "burst_only" {
		return parseExit(misuse(fs, "--quota-tier must be \"guaranteed\" or \"burst_only\", got %s", *quotaTier))
	}
	body := map[string]any{}
	for key, value := range map[string]string{
		"agent_id": *agent, "flavor": *flavor, "model": *model, "name": *name, "role": *role, "quota_tier": *quotaTier,
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
			return parseExit(misuse(fs, "--hyperparameters must be a JSON object, got %s", *hyperparameters))
		}
		body["hyperparameters"] = object
	}
	return call(c, "POST", agentsPath(pe), body, "agents", "no agent was added", stdout, stderr)
}

func agentsRemove(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments agents remove", `usage: hl platform-experiments agents remove [--platform-experiment PE] AGENT

Takes the agent out of a platform experiment that is still open: its token stops working and a
hosted agent's machine is taken down. A coordinator's token removes only an agent it added,
and 403 agent_not_added_by_coordinator answers for any other.
Refused once the experiment has started; from then on an agent leaves by being cut or by the
experiment closing.`, stderr)
	named := experimentFlag(fs)
	agent, err := parseOne(fs, args, "AGENT", "", "agent")
	if err != nil {
		return parseExit(err)
	}
	pe, err := namedExperiment(fs, c, *named)
	if err != nil {
		return parseExit(err)
	}
	return call(c, "DELETE", agentsPath(pe)+"/"+url.PathEscape(agent), nil, "", "", stdout, stderr)
}

func agentsList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments agents list", `usage: hl platform-experiments agents list [--platform-experiment PE]

Every agent that joined the platform experiment, in the order they did, each with its role and
in actor whose credential added it. A draft's planner has role planner. While the platform
experiment is open, every agent but a baseline agent waits for the start and says so in
status_reason, with the reason the experiment is not running yet: no machine is asked for it
before, and its instance.state reads waiting until then. After the start instance.state is
requested once a machine is asked for, starting while it boots, running, and stopped. No token
is listed:
hl platform-experiments agents add --agent ID prints a self-run agent's token again.`, stderr)
	named := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	pe, err := namedExperiment(fs, c, *named)
	if err != nil {
		return parseExit(err)
	}
	return call(c, "GET", agentsPath(pe), nil, "agents", fmt.Sprintf("no agents have joined %s", pe), stdout, stderr)
}

func agentsFinish(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments agents finish", `usage: hl platform-experiments agents finish --reason TEXT

Says that you have nothing left to do in the platform experiment: your quota is spent, or
nothing you could still try is worth a job. Your machine is then stopped after this turn and the
coordinator is told, with the reason. Refused while you have a job queued or running or a
completed job without its summary (work_unfinished), and while a hypothesis you own is still
open (hypotheses_open): the refusal lists them in hypothesis_ids, and you give each its verdict
first with hl hypothesis status ID, inconclusive when nothing settled it. Once the platform experiment closed the
answer is experiment_closed: your run is over already and nothing is left to finish, so end
your turn. It cannot be taken back.`, stderr)
	named := experimentFlag(fs)
	agent := fs.String("agent", "", "agent id (default the token's, else $AGENT_ID)")
	reason := fs.String("reason", "", "why nothing is left to do")
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	if strings.TrimSpace(*reason) == "" {
		return parseExit(misuse(fs, "want --reason"))
	}
	var err error
	if *agent, err = writingAgent(c, *agent); err != nil {
		return fail(stderr, err)
	}
	if *agent == "" {
		return parseExit(misuse(fs, "want --agent: HL_TOKEN names none and $"+agentEnv+" is not set"))
	}
	pe, err := namedExperiment(fs, c, *named)
	if err != nil {
		return parseExit(err)
	}
	return call(c, "POST", agentsPath(pe)+"/"+url.PathEscape(*agent)+"/finish", map[string]string{"reason": *reason}, "", "", stdout, stderr)
}
