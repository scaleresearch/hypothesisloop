package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

// titleSlugLength is where the hypothesis's text is cut in a branch name.
const titleSlugLength = 40

// implementationBranch names the branch one agent's code for one hypothesis lives on:
// hl/<platform experiment>/<agent name>/<start of the hypothesis's text>-<end of its id>. The
// end of the id tells apart two hypotheses that begin alike.
func implementationBranch(platformExperiment, agentName, text, hypothesisID string) (string, error) {
	own, id := gitrefs.AgentBranchPrefix(platformExperiment, agentName), gitrefs.Slug(strings.ReplaceAll(hypothesisID, "-", ""), 0)
	if own == "" || len(id) < 6 {
		return "", fmt.Errorf("agent name %q and hypothesis id %q make no branch name", agentName, hypothesisID)
	}
	title := id[len(id)-6:]
	if words := gitrefs.Slug(text, titleSlugLength); words != "" {
		title = words + "-" + title
	}
	return own + title, nil
}

func HypothesisCheckout(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis checkout", `usage: hl hypothesis checkout ID [--from JOB]

Run in your clone of the platform experiment's repository. Puts it on your branch for
hypothesis ID: your implementation of it. A hypothesis is an idea and a branch is one agent's
code for it, so any agent may check out any hypothesis, its own or another's, and each gets a
branch of its own: hl/PE/<your agent name>/<start of the hypothesis text>-<end of its id>.

It fetches origin first. When your branch exists, here or on origin, it is checked out as it
is. Otherwise it is created: from the commit job JOB ran with --from JOB (any job of the
platform experiment, to start from one particular implementation), else from base_ref of the
hypothesis: the commit the newest completed job of the hypothesis it builds on ran, whichever
agent's, that hypothesis's own base while it has no completed job, and the baseline job's
commit for one that builds on the objective. To drop an idea and start again from the
baseline, register a hypothesis that builds on root and check it out; to improve an idea,
register one that builds on it.

The checkout is recorded as you working on the hypothesis: hl hypothesis get and list show you
in working and the coordinator sees it, for two hours or until a job of yours shows it.
Checking out again renews it. Another agent already on it is no refusal: two approaches to one
idea can both be worth running. Read what they do before you repeat it.

Prints one JSON object: branch; base, the commit a branch it created starts from; and, when
other agents are on the hypothesis now, working, who, and warning. Refused with the reason
when there is nothing to start from (no baseline is declared yet), and when --from is given for
a branch that exists. hl job submit pushes the branch.`, stderr)
	agent := fs.String("agent", "", "whose branch it is, for a person or an API key; an agent's token says it")
	from := fs.String("from", "", "create the branch from the commit this `job` ran")
	id, err := command.ParseOne(fs, args, "ID", "", "id")
	if err != nil {
		return command.ParseExit(err)
	}
	if *agent = c.Agent(*agent); *agent == "" {
		return command.ParseExit(command.Misuse(fs, "--agent is required: HL_TOKEN is no agent's token, so say whose branch this is"))
	}
	if id, err = apiclient.FullHypothesisID(c, c.Experiment(""), id); err != nil {
		return command.Fail(stderr, err)
	}
	var h struct {
		ID                 string `json:"id"`
		PlatformExperiment string `json:"platform_experiment_id"`
		Text               string `json:"text"`
		BaseRef            string `json:"base_ref"`
	}
	if err := c.Do("GET", "/hypotheses/"+url.PathEscape(id)+"?limit=1", nil, &h); err != nil {
		return command.Fail(stderr, err)
	}
	var signup struct {
		Name string `json:"name"`
	}
	if err := c.Do("GET", apiclient.SignupPath(h.PlatformExperiment, *agent), nil, &signup); err != nil {
		return command.Fail(stderr, err)
	}
	branch, err := implementationBranch(h.PlatformExperiment, signup.Name, h.Text, h.ID)
	if err != nil {
		return command.Fail(stderr, fmt.Errorf("hypothesis checkout: %w", err))
	}
	base := h.BaseRef
	if *from != "" {
		var job struct {
			PlatformExperiment string `json:"platform_experiment_id"`
			CodeRef            string `json:"code_ref"`
		}
		if err := c.Do("GET", "/experiments/"+url.PathEscape(*from), nil, &job); err != nil {
			return command.Fail(stderr, err)
		}
		if job.PlatformExperiment != h.PlatformExperiment {
			return command.Fail(stderr, fmt.Errorf("hypothesis checkout: job %s is of platform experiment %s and hypothesis %s of %s: --from takes a job of the same one", *from, job.PlatformExperiment, h.ID, h.PlatformExperiment))
		}
		base = job.CodeRef
	}
	created, err := checkoutBranch("", branch, base, *from)
	if err != nil {
		return command.Fail(stderr, fmt.Errorf("hypothesis checkout: %w", err))
	}
	out := struct {
		Branch  string            `json:"branch"`
		Base    string            `json:"base,omitempty"`
		Working []json.RawMessage `json:"working,omitempty"`
		Warning string            `json:"warning,omitempty"`
	}{Branch: branch, Base: created}
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(h.ID)+"/checkouts", map[string]string{"agent_id": *agent, "branch": branch}, &out); err != nil {
		fmt.Fprintf(stderr, "hl: hypothesis checkout: the branch is checked out and the platform did not record it: %v\n", err)
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

// checkoutBranch puts the clone in dir on branch after fetching origin. A branch the clone or
// origin has is checked out as it is, and from, the job a new one was to start from, is then
// refused. One neither has is created at the commit of base, a code_ref, and that commit is
// returned.
func checkoutBranch(dir, branch, base, from string) (created string, err error) {
	if _, err := git(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", errors.New("this is not a git repository: run it in your clone of the platform experiment's repository (hl platform-experiments get names repository_url)")
	}
	if _, err := git(dir, "fetch", "--quiet", "origin"); err != nil {
		return "", err
	}
	_, localErr := git(dir, "rev-parse", "--quiet", "--verify", "refs/heads/"+branch)
	_, remoteErr := git(dir, "rev-parse", "--quiet", "--verify", "refs/remotes/origin/"+branch)
	if localErr == nil || remoteErr == nil {
		if from != "" {
			return "", fmt.Errorf("your branch %s exists, so nothing is created from job %s: leave --from out to check it out as it is", branch, from)
		}
		args := []string{"checkout", "--quiet", branch}
		if localErr != nil {
			args = []string{"checkout", "--quiet", "-b", branch, "--track", "origin/" + branch}
		}
		return "", switchBranch(dir, args...)
	}
	if base == "" {
		return "", errors.New("there is no commit to start from: the hypothesis builds up to the objective and no baseline is declared yet. Wait for the baseline, or name a job to start from with --from JOB")
	}
	sha := base[strings.LastIndex(base, "@")+1:]
	if err := switchBranch(dir, "checkout", "--quiet", "-b", branch, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// switchBranch runs the git checkout of args. Refused over changes in the clone, it says what to
// do about them in place of git's error.
func switchBranch(dir string, args ...string) error {
	_, err := git(dir, args...)
	if err == nil {
		return nil
	}
	if changes, statusErr := git(dir, "status", "--porcelain"); statusErr == nil && strings.TrimSpace(changes) != "" {
		return errors.New("git will not switch branches over the changes in your clone (git status lists them). " +
			"Commit them (git add -A && git commit) or stash them (git stash -u), then run hl hypothesis checkout again; " +
			"the branch starts from its base, and git cherry-pick or git stash pop brings your changes onto it")
	}
	return err
}
