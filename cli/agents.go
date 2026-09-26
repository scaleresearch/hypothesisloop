package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

var agentsCommands = []subcommand{
	{"add", "create one agent in a platform experiment, or join a registered one with --agent", agentsAdd},
	{"remove", "take an agent out of a platform experiment that has not started", agentsRemove},
	{"list", "the agents of a platform experiment and what each was launched with", agentsList},
}

func agentsPath(platformExperiment string) string {
	return "/platform-experiments/" + url.PathEscape(platformExperiment) + "/agents"
}

func agentsAdd(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments agents add", `usage: hl platform-experiments agents add [PE] [--hosted] [--flavor F] [--hyperparameters JSON] [--model M] [--name NAME] [--role ROLE] [--quota-tier TIER]
   or: hl platform-experiments agents add [PE] --agent ID [--role ROLE] [--quota-tier TIER]

The only way an agent joins a platform experiment (default the token's, else $PLATFORM_EXPERIMENT_ID), and only
while it is open. The first form creates one agent; the second joins one registered with hl
register. For several agents, run it once for each. With --hosted the platform runs the agent.
Without it the line printed carries the agent's token: start the agent yourself with that as
HL_TOKEN beside its AGENT_ID. Keep the token out of
command lines and shell history; write it to an env file the container reads.`, stderr)
	agent := fs.String("agent", "", "join this registered agent instead of creating a new one")
	hosted := fs.Bool("hosted", false, "the platform runs the agent")
	flavor := fs.String("flavor", "", "the specialization the agent runs (AGENT_FLAVOR)")
	hyperparameters := fs.String("hyperparameters", "", "a JSON object handed to the agent (AGENT_HYPERPARAMETERS)")
	model := fs.String("model", "", "the model the agent runs on (AGENT_MODEL); omitted, the platform's default")
	name := fs.String("name", "", "display name of a created agent; omitted, its generated id")
	kind := fs.String("kind", "", "kind of a created agent: agent (default) or human")
	role := fs.String("role", "", "competitor (default), baseline or reviewer")
	quotaTier := fs.String("quota-tier", "", "guaranteed or burst_only; omit to use the platform experiment's default_quota_tier")
	pe, err := parseOne(fs, args, "PE", experimentOf(c, ""), "platform-experiment")
	if err != nil {
		return parseExit(err)
	}
	if *quotaTier != "" && *quotaTier != "guaranteed" && *quotaTier != "burst_only" {
		return parseExit(misuse(fs, "--quota-tier must be \"guaranteed\" or \"burst_only\", got %s", *quotaTier))
	}
	body := map[string]any{}
	for key, value := range map[string]string{
		"agent_id": *agent, "flavor": *flavor, "model": *model, "name": *name, "kind": *kind, "role": *role, "quota_tier": *quotaTier,
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
	fs := newFlags("platform-experiments agents remove", `usage: hl platform-experiments agents remove PE AGENT

Takes the agent out of a platform experiment that is still open: its token stops working and a
hosted agent's machine is taken down. Refused once the experiment has started; from then on an
agent leaves by being cut or by the experiment closing.`, stderr)
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) != 2 {
		return parseExit(misuse(fs, "want PE and AGENT, got %d operands", len(operands)))
	}
	return call(c, "DELETE", agentsPath(operands[0])+"/"+url.PathEscape(operands[1]), nil, "", "", stdout, stderr)
}

func agentsList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("platform-experiments agents list", `usage: hl platform-experiments agents list [PE]

Every agent that joined the platform experiment (default the token's, else $PLATFORM_EXPERIMENT_ID), in the order
they did. No token is listed: hl platform-experiments agents add PE --agent ID prints a self-run
agent's token again.`, stderr)
	pe, err := parseOne(fs, args, "PE", experimentOf(c, ""), "platform-experiment")
	if err != nil {
		return parseExit(err)
	}
	return call(c, "GET", agentsPath(pe), nil, "agents", fmt.Sprintf("no agents have joined %s", pe), stdout, stderr)
}
