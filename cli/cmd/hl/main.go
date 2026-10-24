// A job of hl is a domain.Experiment: the API, the event kinds and experiment_id say experiment
// where hl's commands and flags say job, for the same row.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
	"github.com/scaleresearch/hypothesisloop/contract/agentsandbox"

	"github.com/scaleresearch/hypothesisloop/cli/internal/agent"
	"github.com/scaleresearch/hypothesisloop/cli/internal/people"
	"github.com/scaleresearch/hypothesisloop/cli/internal/shared"
)

func main() {
	if filepath.Base(os.Args[0]) == agent.CredentialHelperName {
		client := apiclient.New(os.Getenv(agentsandbox.EnvAPIURL), os.Getenv(agentsandbox.EnvToken))
		os.Exit(agent.CredentialHelper(client, os.Args[1:], os.Stdin, os.Stdout))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	token := os.Getenv(agentsandbox.EnvToken)
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage(token))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, usage(token))
		return 0
	}

	client := apiclient.New(os.Getenv(agentsandbox.EnvAPIURL), token)
	for _, cmd := range commands {
		if cmd.Name != args[0] {
			continue
		}
		return cmd.Runner(cmd.Name)(client, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "hl: unknown command %q\n\n%s\n", args[0], usage(token))
	return 2
}

// commands is the one table of hl: a command is a command.Func of the package of those who run
// it, and a row here. One with Subs is a group, listed as the commands of it its reader runs,
// then its about. It is filled in init because the group wrappers below read it.
var commands []command.Command

func init() {
	commands = []command.Command{
		{Name: "platform-experiments", Run: platformExperiments, Subs: []command.Command{
			{Name: "list", About: "the platform experiments this token reaches, one line each", Run: shared.PlatformExperimentsList},
			{Name: "get", About: "the platform experiment: its status, what it waits for, its description and its stages", Run: shared.PlatformExperimentsGet},
			{Name: "health", About: "where each agent and the coordinator last stood: first job, jobs in flight, restarts, messages", Run: shared.PlatformExperimentsHealth},
			{Name: "start", About: "push a directory to a repository and create the platform experiment its spec states", Run: people.PlatformExperimentsStart},
			{Name: "update", About: "change the description or other fields, or declare or raise the baseline", Run: shared.PlatformExperimentsUpdate},
			{Name: "propose", About: "a draft's planner: propose the experiment the draft becomes", Run: agent.PlatformExperimentsPropose},
			{Name: "decline", About: "a draft's planner: say why no experiment is proposed", Run: agent.PlatformExperimentsDecline},
			{Name: "close", About: "end the platform experiment for good", Run: shared.PlatformExperimentsClose},
			{Name: "reopen", About: "set a closed platform experiment running again until TIME, as it was", Run: people.PlatformExperimentsReopen},
			{Name: "summary", About: "write the summary of the platform experiment", Run: shared.PlatformExperimentsSummary},
			{Name: "agents", About: "the agents of a platform experiment", Subs: []command.Command{
				{Name: "add", About: "create one agent in a platform experiment, or join a registered one with --agent", Run: shared.AgentsAdd},
				{Name: "remove", About: "take an agent out of a platform experiment that has not started", Run: shared.AgentsRemove},
				{Name: "list", About: "the agents of a platform experiment and what each was launched with", Run: shared.AgentsList},
				{Name: "finish", About: "say that you, an agent, have nothing left to do: your machine stops", Run: agent.AgentsFinish},
				{Name: "message", About: "tell an agent, with --all every executor and researcher or with --coordinator the coordinator, a direction directly: it is woken with it", Run: shared.AgentsMessage},
				{Name: "messages", About: "the messages sent to agents, oldest first; an agent's token reads its own", Run: shared.AgentsMessages},
				{Name: "session", About: "the stored model session of an agent, the coordinator or the planner", Run: shared.AgentsSession},
			}},
		}},
		{Name: "quota", About: "your share of the budget and what a job submitted now may still hold of it; --all for every agent", Run: shared.Quota},
		{Name: "results", About: "standings on the declared metrics", Run: shared.Results},
		{Name: "capacity", About: "live accelerators per cluster, available and total", Run: shared.Capacity},
		{Name: "situation", About: "the run now: time, objective, capacity, budget and each agent's work and allowances", Run: shared.Situation},
		{Name: "models", About: "the models an agent or coordinator can run on, as <provider>/<model id>, with context length and price; --search, --provider", Run: shared.Models},
		{Name: "hypothesis", Run: hypothesis, Subs: []command.Command{
			{Name: "submit", About: "register an idea under the objective or the hypotheses it builds on", Run: shared.HypothesisSubmit},
			{Name: "checkout", About: "put the clone on your branch for a hypothesis, made from what the hypothesis builds on, and record you as working on it", Run: agent.HypothesisCheckout},
			{Name: "link", About: "add a link from your own hypothesis, or from any as a researcher", Run: shared.HypothesisLink},
			{Name: "unlink", About: "remove a link from your own hypothesis, or from any as a researcher; a hypothesis keeps one builds_on", Run: shared.HypothesisUnlink},
			{Name: "merge", About: "a researcher's: merge an open duplicate into the hypothesis that states its claim, its incoming links moved there", Run: shared.HypothesisMerge},
			{Name: "retire", About: "a researcher's: take an open idea off the frontier with the reason; it takes no job and is no verdict", Run: shared.HypothesisRetire},
			{Name: "comment", About: "add a note to a hypothesis, or to the objective as root", Run: shared.HypothesisComment},
			{Name: "status", About: "set the verdict of your hypothesis or of a person's idea you tested; confirmed and refuted name the control (--against) and need two completed jobs of its own besides it, one where every ranking metric is deterministic, or --despite with why it holds without", Run: shared.HypothesisStatus},
			{Name: "list", About: "hypotheses, open ideas of people first, then most recent", Run: shared.HypothesisList},
			{Name: "get", About: "one hypothesis with its links, findings, implementations, comments and jobs, short unless --full; root: the comments on the objective", Run: shared.HypothesisGet},
			{Name: "query", About: "read-only Cypher over the graph, including its text search", Run: shared.HypothesisQuery},
		}},
		{Name: "job", Subs: []command.Command{
			{Name: "submit", About: "submit a job spec, prints its id", Run: agent.JobSubmit},
			{Name: "list", About: "jobs, newest first, one brief line each with its metrics", Run: shared.JobList},
			{Name: "status", About: "a job's whole record", Run: shared.JobStatus},
			{Name: "metrics", About: "per metric: last value, min, max and points reported", Run: shared.JobMetrics},
			{Name: "summary", About: "print a job's write-up, or file it with --text", Run: shared.JobSummary},
			{Name: "wait", About: "block until jobs end, then print each as list does", Run: shared.JobWait},
			{Name: "logs", About: "the last log lines its cluster reported", Run: shared.JobLogs},
			{Name: "data", About: "what the job wrote to the object store", Run: shared.JobData},
			{Name: "cost", About: "what it cost so far, against the estimate", Run: shared.JobCost},
			{Name: "cancel", About: "cancel a job that has not ended, releasing what it holds of its agent's quota", Run: shared.JobCancel},
			{Name: "stats", About: "job counts by status, capacity tier and eviction reason", Run: shared.JobStats},
		}},
		{Name: "image", About: "the image the platform experiment's jobs run in", Subs: []command.Command{
			{Name: "show", About: "the image recorded for the jobs, by digest, and their architecture", Run: agent.ImageShow},
			{Name: "set", About: "record the image every job submitted from now on runs in", Run: agent.ImageSet},
			{Name: "repository", About: "where the experiment's images are pushed; podman gets its login from hl", Run: agent.ImageRepository},
		}},
		{Name: "repo", About: "the files every experiment's repository holds", Subs: []command.Command{
			{Name: "init", About: "put the experiment template in a new directory", Run: people.RepoInit},
			{Name: "check", About: "check that this clone holds the experiment files, each valid and fitting the platform experiment", Run: agent.RepoCheck},
		}},
		{Name: "org", Subs: []command.Command{
			{Name: "list", About: "the orgs this token reaches, with your role in each", Run: people.OrgList},
			{Name: "create", About: "create an org you own, with its default project", Run: people.OrgCreate},
			{Name: "rename", About: "rename an org; its id stays", Run: people.OrgRename},
			{Name: "members", About: "the people of an org: list them, change a role, remove one or leave", Subs: []command.Command{
				{Name: "list", About: "an org's members and the role each holds", Run: people.OrgMembersList},
				{Name: "set-role", About: "make a member an owner, or an owner a member", Run: people.OrgMembersSetRole},
				{Name: "remove", About: "remove a member, or leave by naming yourself", Run: people.OrgMembersRemove},
			}},
			{Name: "invites", About: "invite people into an org by a token, and accept one", Subs: []command.Command{
				{Name: "create", About: "create an invite; its token is printed once and never again", Run: people.OrgInvitesCreate},
				{Name: "list", About: "an org's pending invites, never their tokens", Run: people.OrgInvitesList},
				{Name: "revoke", About: "end an invite that was not accepted", Run: people.OrgInvitesRevoke},
				{Name: "accept", About: "join the org an invite is for, as the person signed in", Run: people.OrgInvitesAccept},
			}},
		}},
		{Name: "project", Subs: []command.Command{
			{Name: "list", About: "an org's projects", Run: people.ProjectList},
			{Name: "create", About: "create a project in an org", Run: people.ProjectCreate},
			{Name: "rename", About: "rename a project; its id stays", Run: people.ProjectRename},
		}},
		{Name: "api-key", Subs: []command.Command{
			{Name: "create", About: "create an API key; its token is printed once and never again", Run: people.ApiKeyCreate},
			{Name: "list", About: "an org's API keys, revoked ones included, never their tokens", Run: people.ApiKeyList},
			{Name: "revoke", About: "end an API key and every key minted from it", Run: people.ApiKeyRevoke},
		}},
		{Name: "cluster", Subs: []command.Command{
			{Name: "add", About: "register a cluster for an org and print the command that starts its agent", Run: people.ClusterAdd},
			{Name: "list", About: "the clusters an org's jobs run on, whether each is connected and whether the org has it enabled", Run: people.ClusterList},
			{Name: "token", About: "print the runtime token of a cluster the org registered", Run: people.ClusterToken},
			{Name: "order", About: "set the order the org's jobs are placed on the clusters it registered, first placed first", Run: people.ClusterOrder},
			{Name: "settings", About: "set a cluster's own scale-up timeout, speculative cap or stuck-pending timeout in place of the scheduler's defaults", Run: people.ClusterSettings},
			{Name: "disable", About: "turn a cluster off for the org's jobs: none is placed there, the ones running finish", Run: people.ClusterDisable},
			{Name: "enable", About: "turn a cluster on again for the org's jobs", Run: people.ClusterEnable},
			{Name: "forget-identity", About: "let a cluster whose machines were rebuilt bind to the identity they report now", Run: people.ClusterForgetIdentity},
			{Name: "remove", About: "end a cluster: requeue its jobs and refuse its runtime token", Run: people.ClusterRemove},
		}},
		{Name: "watch", About: "your own updates, one JSON object per line", Run: shared.Watch},
	}
}

// subsOf is the commands of the group name.
func subsOf(name string) []command.Command {
	for _, cmd := range commands {
		if cmd.Name == name {
			return cmd.Subs
		}
	}
	return nil
}

// platformExperiments is the group, with list as what it does when no command is named.
func platformExperiments(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && args[0] != "-h" && args[0] != "--help") {
		return shared.PlatformExperimentsList(c, args, stdout, stderr)
	}
	return command.Group("platform-experiments", subsOf("platform-experiments"), c, args, stdout, stderr)
}

func hypothesis(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "comments" || args[0] == "links") {
		fmt.Fprintf(stderr, "hl: hypothesis %s: read them with hl hypothesis get ID, add one with hl hypothesis %s\n", args[0], strings.TrimSuffix(args[0], "s"))
		return 2
	}
	return command.Group("hypothesis", subsOf("hypothesis"), c, args, stdout, stderr)
}

const usageRules = `
An ID is positional (--id ID works too). A command given a job's or a hypothesis's ID takes no
--platform-experiment; every other one in a platform experiment takes it as a flag, never an
operand. --agent and --platform-experiment default to what an agent's or a coordinator's token
names; a person or an API key writes both out. Where --agent filters a list it has no default.`

const usageOfPeople = ` --org defaults to $HL_ORG and
then to the one org the token reaches, --project to $HL_PROJECT. hl job --help, hl hypothesis
--help and the --help of org, project, api-key and cluster list their commands with every flag.`

// usage lists the commands of hl.
func usage(token string) string {
	var b strings.Builder
	b.WriteString("usage: hl <command> ...\n\n")
	for _, cmd := range commands {
		about := cmd.About
		if cmd.Subs != nil {
			var names []string
			for _, sub := range cmd.Subs {
				names = append(names, sub.Name)
			}
			about = strings.TrimSuffix(strings.Join(names, " | ")+": "+about, ": ")
		}
		fmt.Fprintf(&b, "  %-22s%s\n", cmd.Name, about)
	}
	b.WriteString(usageRules)
	if apiclient.KindOf(token) == apiclient.CallerOther {
		b.WriteString(usageOfPeople)
	}
	return b.String()
}
