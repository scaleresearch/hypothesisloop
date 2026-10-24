package agent

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func JobSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job submit", `usage: hl job submit [FILE|-] [--hypothesis ID] [--theory T | --theory-file FILE|-] [--env K=V ...] [--estimated-duration-hours H] [--capacity-tier guaranteed|burst] [--secret-env NAME ...] [--repeat N]

FILE is a YAML job spec (metadata, job). The flags override it for this submission, so one
committed spec serves every job that differs in hypothesis, env, estimate or tier. Prints the
job's id, status, code_ref, branch and estimates; hl job status ID has the rest. The result
arrives as an update with its metrics, unasked: while your quota has room, submit the next
independent trial beside it. same_hypothesis names other agents' jobs on the same hypothesis.
not_on_best says the job's commit does not contain the run's best job on its first ranking
metric, naming that job, its value and its commit, which this checkout fetches from origin to
tell. Either way the job is queued: stacking on another base may be meant.
A submit the same in every part (agent, hypothesis, commit, FILE, flags) is one job however
often it is sent, so one that timed out is safe to resend; --repeat N makes it the Nth job of
that request.

Quota: a job holds its estimated_cost_acch from submit until it ends, then what it was billed
(hl job cost ID). With no estimate in FILE or --estimated-duration-hours it gets
job_estimated_duration_hours of the experiment, the baseline job's run time plus a half; a
baseline agent's job gets the largest your quota admits that ends by baseline_due_at. A submit
refused for quota fits once a job of yours ends, or with a lower estimate that is still no less
than the job takes. Burst runs on idle capacity and can be evicted for a guaranteed job; it
draws on the same quota.

Required: metadata.hypothesis_id (or --hypothesis), any agent's registered hypothesis (a
hypothesis and a branch are two things), and metadata.theory (or --theory), what you predict
the job will show. A baseline agent's job names neither (baseline_tests_no_hypothesis) and runs
on branch hl/PE/baseline (git checkout -b hl/PE/baseline), or on hl/PE/baseline-check to try
the code as an executor would. Only a job of hl/PE/baseline is declared the baseline.
--secret-env NAME sends $NAME from this environment as job.secret_env, which no read of the
job shows; FILE may not carry job.secret_env. An agent's token says whose job it is and where;
a person or an API key states --agent and --platform-experiment.

Code: commit first (uncommitted changes to tracked files are refused). HEAD must be on a branch
under hl/PE/ (hl hypothesis checkout ID puts you on yours). The submit fills an empty
metadata.code_ref with <origin url>@<HEAD sha> and metadata.branch with that branch, and pushes
the branch itself. What a job ran is its code_ref. Every node fetches that commit into
/workspace/code and runs the command there, in the image the platform experiment records
(hl image show), on nodes of its architecture. The job records the image's digest.

Refusals: required (job.cpu, job.memory, job.storage or metadata.estimated_duration_hours left
out, the field named in problems); malformed (no job.command); no_experiment_image (none recorded:
hl image set); exceeds_cluster_capacity (more than any cluster has idle, with both figures);
no_cluster_for_architecture (no cluster has nodes of the image's architecture: hl image set one
built for theirs);
exceeds_project_compute_limit (the job alone reserves more of the platform's clusters than
its project may hold and no cluster of the org's own can hold it: hl capacity shows the limit);
experiment_not_started (before the start only a baseline agent's job is taken; see status_reason
of hl platform-experiments get); experiment_closed.

  metadata:
    hypothesis_id: hyp-456        # from 'hl hypothesis submit'
  job:
    command: ["python", "train.py"]   # runs in /workspace/code, the commit you submit from
    accelerator_type: ...         # a job on CPU leaves these two out and holds its cpu, whole cores
    accelerator_count: 1
    cpu: 2                        # per node, like memory and storage, and in each group of a
    memory: 8Gi                   # job with groups: a job holds what it states
    storage: 4Gi                  # scratch space
                                  # max_retries left out is 0

Every other field: $HL_REFERENCE/experiment-submission.yaml.`, stderr)
	agent := fs.String("agent", "", "whose job it is, for a person or an API key; an agent's token says it")
	overrides := map[string]*string{
		"hypothesis_id":          fs.String("hypothesis", "", "sets metadata.hypothesis_id"),
		"platform_experiment_id": fs.String("platform-experiment", "", "sets metadata.platform_experiment_id (default the file's, then the token's)"),
		"capacity_tier":          fs.String("capacity-tier", "", "sets metadata.capacity_tier: guaranteed or burst"),
	}
	theoryFlag := command.NewTextFlag(fs, "theory", "sets metadata.theory")
	estimate := fs.String("estimated-duration-hours", "", "sets metadata.estimated_duration_hours, the `hours` this job is expected to run")
	repeat := fs.Int("repeat", 1, "which job of this same request it is: 2 for a second run of what was already submitted, `N` for the Nth")
	var env command.StringList
	fs.Var(&env, "env", "job.env entry as `K=V`, repeat per variable")
	var secretEnv command.StringList
	fs.Var(&secretEnv, "secret-env", "job.secret_env entry by `NAME`, its value read from $NAME here; repeat per variable")
	path, err := command.ParseOne(fs, args, "FILE", "-")
	if err != nil {
		return command.ParseExit(err)
	}
	if *agent = c.Agent(*agent); *agent == "" {
		return command.ParseExit(command.Misuse(fs, "--agent is required: HL_TOKEN is no agent's token, so say whose job this is"))
	}
	theory, err := theoryFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if path == "-" {
		if err := fs.ClaimStdin("FILE - (the job spec, read from stdin when no FILE is named)"); err != nil {
			return command.ParseExit(err)
		}
	}
	body, err := command.ReadYAMLBody(path)
	if err != nil {
		fmt.Fprintf(stderr, "hl: %s\n", err)
		return 2
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	for key, value := range overrides {
		if *value != "" {
			metadata[key] = *value
		}
	}
	if theory != "" {
		metadata["theory"] = theory
	}
	if *estimate != "" {
		hours, err := strconv.ParseFloat(*estimate, 64)
		if err != nil || hours <= 0 {
			return command.ParseExit(command.Misuse(fs, "--estimated-duration-hours wants a positive number of hours, got %q", *estimate))
		}
		metadata["estimated_duration_hours"] = hours
	}
	if err := setJobEnv(body, env); err != nil {
		return command.ParseExit(command.Misuse(fs, "%s", err))
	}
	if err := setJobSecretEnv(body, secretEnv); err != nil {
		return command.ParseExit(command.Misuse(fs, "%s", err))
	}
	metadata["agent_id"] = *agent
	if v := metadata["platform_experiment_id"]; v == nil || v == "" {
		metadata["platform_experiment_id"] = c.Experiment("")
	}
	if id, _ := metadata["hypothesis_id"].(string); id != "" {
		pe, _ := metadata["platform_experiment_id"].(string)
		if metadata["hypothesis_id"], err = apiclient.FullHypothesisID(c, pe, id); err != nil {
			return command.Fail(stderr, fmt.Errorf("job submit: %w", err))
		}
	}
	if err := fillCodeRef(metadata, ""); err != nil {
		return command.Fail(stderr, fmt.Errorf("job submit: %w", err))
	}
	body["metadata"] = metadata
	if *repeat < 1 {
		return command.ParseExit(command.Misuse(fs, "--repeat wants 1 or more, got %d", *repeat))
	}
	if body["submission_key"], err = submissionKey(body, *repeat); err != nil {
		return command.Fail(stderr, fmt.Errorf("job submit: %w", err))
	}
	var answer struct {
		submitted
		RunBest *runBest `json:"run_best"`
	}
	if err := c.Do("POST", "/experiments", body, &answer); err != nil {
		return command.Fail(stderr, err)
	}
	out := answer.submitted
	if answer.RunBest != nil {
		out.NotOnBest = notOnBest("", out.ID, out.CodeRef, *answer.RunBest)
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	// A job this submit created is QUEUED in its answer, so any other status is that of the job
	// an identical submit created before.
	if out.Status != string(platformapi.StatusQueued) {
		again := fmt.Sprintf("--repeat %d submits the same request as another job", *repeat+1)
		if platformapi.ExperimentStatus(out.Status).Ended() {
			fmt.Fprintf(stderr, "hl: job submit: nothing was submitted: an identical submit already created %s, which ended %s. %s\n", out.ID, out.Status, again)
			return 1
		}
		fmt.Fprintf(stderr, "hl: job submit: an identical submit already created %s, which is %s, and nothing more was submitted. %s\n", out.ID, out.Status, again)
	}
	return 0
}

// submissionKey names a submit by everything it sends and which repeat of it this is, so that
// the same submit sent twice is one job on the platform (submission_key of POST /experiments).
func submissionKey(body map[string]any, repeat int) (string, error) {
	request, err := json.Marshal(map[string]any{"request": body, "repeat": repeat})
	if err != nil {
		return "", fmt.Errorf("encoding the job: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(request)), nil
}

// setJobEnv applies K=V pairs onto body's job.env.
func setJobEnv(body map[string]any, pairs []string) error {
	if len(pairs) == 0 {
		return nil
	}
	job, ok := body["job"].(map[string]any)
	if !ok {
		return errors.New("--env needs a job mapping in FILE")
	}
	env, ok := job["env"].(map[string]any)
	if !ok {
		if job["env"] != nil {
			return errors.New("--env needs job.env in FILE to be a mapping")
		}
		env = map[string]any{}
		job["env"] = env
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return fmt.Errorf("--env wants K=V, got %q", pair)
		}
		env[key] = value
	}
	return nil
}

// setJobSecretEnv sets body's job.secret_env to the named variables of this process's environment.
// A value comes from nowhere else: not from the command line, which other users of the machine
// can read, and not from the spec file, which is committed.
func setJobSecretEnv(body map[string]any, names []string) error {
	job, _ := body["job"].(map[string]any)
	if _, inFile := job["secret_env"]; inFile {
		return errors.New("FILE carries job.secret_env: a job spec is a committed file, pass each secret as --secret-env NAME with $NAME set")
	}
	if len(names) == 0 {
		return nil
	}
	if job == nil {
		return errors.New("--secret-env needs a job mapping in FILE")
	}
	secret := map[string]any{}
	for _, name := range names {
		value := os.Getenv(name)
		if value == "" {
			return fmt.Errorf("--secret-env %s: $%s is not set in this environment", name, name)
		}
		secret[name] = value
	}
	job["secret_env"] = secret
	return nil
}

// submitted is what hl job submit prints of the reply, which repeats the submitted spec: only
// what the platform decided, and whether the job's commit contains the run's best.
type submitted struct {
	ID             string  `json:"id"`
	Status         string  `json:"status,omitempty"`
	CodeRef        string  `json:"code_ref,omitempty"`
	Branch         string  `json:"branch,omitempty"`
	CapacityTier   string  `json:"capacity_tier,omitempty"`
	EstimatedHours float64 `json:"estimated_duration_hours,omitempty"`
	EstimatedCost  float64 `json:"estimated_cost_acch,omitempty"`
	SameHypothesis string  `json:"same_hypothesis,omitempty"`
	NotOnBest      string  `json:"not_on_best,omitempty"`
}
