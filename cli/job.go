package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/watchclient"
)

var jobCommands = []subcommand{
	{"submit", "submit a job spec, prints its id", jobSubmit, asAgent},
	{"list", "jobs, newest first, one brief line each with its metrics", jobList, everyone},
	{"status", "a job's whole record", jobStatus, everyone},
	{"metrics", "per metric: last value, min, max and points reported", jobMetrics, everyone},
	{"summary", "print a job's write-up, or file it with --text", jobSummary, everyone},
	{"wait", "block until jobs end, then print each as list does", jobWait, everyone},
	{"logs", "the last log lines its cluster reported", jobLogs, everyone},
	{"data", "what the job wrote to the object store", func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return jobRead(c, "data", "/data", "What the job wrote under its data prefix, one object per line, each with a url a plain GET reads it by for 12 hours: how a job of a prior run is read", args, stdout, stderr)
	}, everyone},
	{"cost", "what it cost so far, against the estimate", func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return jobRead(c, "cost", "/cost", "Actual consumption so far (settled once final) beside the admission-time estimate", args, stdout, stderr)
	}, everyone},
	{"cancel", "cancel a job that has not ended, releasing what it holds of its agent's quota", jobCancel, everyone},
	{"stats", "job counts by status, capacity tier and eviction reason", jobStats, everyone},
}

func jobSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job submit", `usage: hl job submit [FILE|-] [--hypothesis ID] [--theory T] [--env K=V ...] [--estimated-duration-hours H] [--capacity-tier guaranteed|burst] [--secret-env NAME ...] [--repeat N]

FILE is a YAML job spec (metadata, job). The flags override it for this submission, so one
committed spec serves every job that differs in hypothesis, env, estimate or tier. Prints the
job's id, status, code_ref, branch and estimates; hl job status ID has the rest.
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
the code as a researcher would. Only a job of hl/PE/baseline is declared the baseline.
--secret-env NAME sends $NAME from this environment as job.secret_env, which no read of the
job shows; FILE may not carry job.secret_env. An agent's token says whose job it is and where;
a person or an API key states --agent and --platform-experiment.

Code: commit first (uncommitted changes to tracked files are refused). HEAD must be on a branch
under hl/PE/ (hl hypothesis checkout ID puts you on yours). The submit fills an empty
metadata.code_ref with <origin url>@<HEAD sha> and metadata.branch with that branch, and pushes
the branch itself. What a job ran is its code_ref. A commit holding .hypothesisloop/Dockerfile
runs in the environment built from it: QUEUED as environment_building until built, REJECTED
as environment_failed with the build's output when the file failed, EVICTED as
environment_unbuilt at no cost when the platform could not build it (submit again).

Refusals: image_required; unknown_job_image (neither a name from hl capacity nor a full
address); no_cluster_for_image; no_environment_builder (the commit declares an environment and
no cluster your org may use builds); cpu_required, memory_required, storage_required;
exceeds_cluster_capacity (more than any cluster has idle, with both figures);
experiment_not_started (before the start only a baseline agent's job is taken; see status_reason
of hl platform-experiments get); experiment_closed.

  metadata:
    hypothesis_id: hyp-456        # from 'hl hypothesis submit'
  job:
    image: python                 # a job_image name from 'hl capacity', or a full address, host first
    command: ["bash", "-c", "dir=$(hl-clone) && cd \"$dir\" && exec python train.py"]
                                  # hl-clone checks out the commit you submit from and prints where
    accelerator_type: ...         # a job on CPU leaves these two out and holds its cpu, whole cores
    accelerator_count: 1
    cpu: 2                        # per node, like memory and storage, and in each group of a
    memory: 8Gi                   # job with groups: a job holds what it states
    storage: 4Gi                  # scratch space
                                  # max_retries left out is 0

Every other field: $WORKLOAD_SAMPLES/experiment-submission.yaml. $WORKLOAD_SAMPLES also holds
whole jobs to read first: each sample's seed/ reports its metrics and its
.hypothesisloop/job.yaml runs it.`, stderr)
	agent := fs.String("agent", "", "whose job it is, for a person or an API key; an agent's token says it")
	overrides := map[string]*string{
		"hypothesis_id":          fs.String("hypothesis", "", "sets metadata.hypothesis_id"),
		"theory":                 fs.String("theory", "", "sets metadata.theory"),
		"platform_experiment_id": fs.String("platform-experiment", "", "sets metadata.platform_experiment_id (default the file's, then the token's)"),
		"capacity_tier":          fs.String("capacity-tier", "", "sets metadata.capacity_tier: guaranteed or burst"),
	}
	estimate := fs.String("estimated-duration-hours", "", "sets metadata.estimated_duration_hours, the `hours` this job is expected to run")
	repeat := fs.Int("repeat", 1, "which job of this same request it is: 2 for a second run of what was already submitted, `N` for the Nth")
	var env stringList
	fs.Var(&env, "env", "job.env entry as `K=V`, repeat per variable")
	var secretEnv stringList
	fs.Var(&secretEnv, "secret-env", "job.secret_env entry by `NAME`, its value read from $NAME here; repeat per variable")
	path, err := parseOne(fs, args, "FILE", "-")
	if err != nil {
		return parseExit(err)
	}
	if *agent = agentOf(c, *agent); *agent == "" {
		return parseExit(misuse(fs, "--agent is required: HL_TOKEN is no agent's token, so say whose job this is"))
	}
	body, err := readYAMLBody(path)
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
	if *estimate != "" {
		hours, err := strconv.ParseFloat(*estimate, 64)
		if err != nil || hours <= 0 {
			return parseExit(misuse(fs, "--estimated-duration-hours wants a positive number of hours, got %q", *estimate))
		}
		metadata["estimated_duration_hours"] = hours
	}
	if err := setJobEnv(body, env); err != nil {
		return parseExit(misuse(fs, "%s", err))
	}
	if err := setJobSecretEnv(body, secretEnv); err != nil {
		return parseExit(misuse(fs, "%s", err))
	}
	metadata["agent_id"] = *agent
	if v := metadata["platform_experiment_id"]; v == nil || v == "" {
		metadata["platform_experiment_id"] = experimentOf(c, "")
	}
	if id, _ := metadata["hypothesis_id"].(string); id != "" {
		pe, _ := metadata["platform_experiment_id"].(string)
		if metadata["hypothesis_id"], err = fullHypothesisID(c, pe, id); err != nil {
			return fail(stderr, fmt.Errorf("job submit: %w", err))
		}
	}
	if err := fillCodeRef(metadata, ""); err != nil {
		return fail(stderr, fmt.Errorf("job submit: %w", err))
	}
	body["metadata"] = metadata
	if *repeat < 1 {
		return parseExit(misuse(fs, "--repeat wants 1 or more, got %d", *repeat))
	}
	if body["submission_key"], err = submissionKey(body, *repeat); err != nil {
		return fail(stderr, fmt.Errorf("job submit: %w", err))
	}
	// The reply repeats the submitted spec; only what the platform decided is printed.
	var out struct {
		ID             string  `json:"id"`
		Status         string  `json:"status,omitempty"`
		CodeRef        string  `json:"code_ref,omitempty"`
		Branch         string  `json:"branch,omitempty"`
		CapacityTier   string  `json:"capacity_tier,omitempty"`
		EstimatedHours float64 `json:"estimated_duration_hours,omitempty"`
		EstimatedCost  float64 `json:"estimated_cost_acch,omitempty"`
	}
	if err := c.Do("POST", "/experiments", body, &out); err != nil {
		return fail(stderr, err)
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	// A job this submit created is QUEUED in its answer, so any other status is that of the job
	// an identical submit created before.
	if out.Status != "QUEUED" {
		again := fmt.Sprintf("--repeat %d submits the same request as another job", *repeat+1)
		if slices.Contains(endedStatuses, out.Status) {
			fmt.Fprintf(stderr, "hl: job submit: nothing was submitted: an identical submit already created %s, which ended %s. %s\n", out.ID, out.Status, again)
			return 1
		}
		fmt.Fprintf(stderr, "hl: job submit: an identical submit already created %s, which is %s, and nothing more was submitted: %s. %s\n", out.ID, out.Status, resultArrives, again)
		return 0
	}
	fmt.Fprintln(stderr, "hl: job submit: "+resultArrives)
	return 0
}

// endedStatuses are the statuses a job ends in.
var endedStatuses = []string{"COMPLETED", "FAILED", "EVICTED", "REJECTED", "CANCELLED"}

// submissionKey names a submit by everything it sends and which repeat of it this is, so that
// the same submit sent twice is one job on the platform (submission_key of POST /experiments).
func submissionKey(body map[string]any, repeat int) (string, error) {
	request, err := json.Marshal(map[string]any{"request": body, "repeat": repeat})
	if err != nil {
		return "", fmt.Errorf("encoding the job: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(request)), nil
}

// resultArrives is what a submit says of the job's result. An agent told only that it arrives once
// it ends its turn ended its turn after every submit, and ran one job at a time on idle machines.
const resultArrives = "the result arrives as an update with its metrics, unasked: while your quota has room, submit the next independent trial beside it, and end your turn once nothing else is worth a job"

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

// jobDrop leaves out of a printed job what a reader already has: the hypothesis text (hl
// hypothesis get), the admission-resolved copy of the submitted spec and the ranking metric, which
// metrics repeats.
var jobDrop = []string{"hypothesis", "resolved_job", "metric"}

// jobBrief is what a listing prints of a job; hl job status ID prints the whole record.
type jobBrief struct {
	ID                 string          `json:"id"`
	ParentID           string          `json:"parent_id"`
	AgentID            string          `json:"agent_id"`
	HypothesisID       string          `json:"hypothesis_id"`
	Objective          bool            `json:"objective,omitempty"`
	Status             string          `json:"status"`
	Metrics            briefMetrics    `json:"metrics"`
	Machine            machineTag      `json:"ran_on"`
	NotAdmittedReason  string          `json:"not_admitted_reason"`
	NotAdmittedMessage string          `json:"not_admitted_message"`
	EvictionReason     string          `json:"eviction_reason"`
	EnvironmentKey     string          `json:"environment_key"`
	PhaseDetail        json.RawMessage `json:"phase_detail"`
	Theory             string          `json:"theory"`
	EstimatedCost      float64         `json:"estimated_cost_acch"`
	CreatedAt          string          `json:"created_at"`
}

// jobResult is what hl hypothesis get prints of a job unless told --full.
type jobResult struct {
	ID      string       `json:"id"`
	AgentID string       `json:"agent_id"`
	Status  string       `json:"status"`
	Metrics briefMetrics `json:"metrics"`
}

// briefMetrics prints a job's metrics as name: value, at six significant digits, with the basis
// when it is not raw. The value is the one the job is read by, which the metric's role decides.
type briefMetrics []struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Basis string  `json:"basis"`
}

func (m briefMetrics) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, metric := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%q:{"value":%s`, metric.Name, strconv.FormatFloat(metric.Value, 'g', 6, 64))
		if metric.Basis != "raw" {
			fmt.Fprintf(&b, `,"basis":%q`, metric.Basis)
		}
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// machineTag prints where a job ran as one short string: the physical machine of a rented
// instance, else the node, then the accelerator's model or, without one, the CPU's. Two jobs
// with different tags are not the same measurement.
type machineTag struct {
	Node    string `json:"node"`
	Machine struct {
		HostID           string `json:"host_id"`
		AcceleratorModel string `json:"accelerator_model"`
		CPUModel         string `json:"cpu_model"`
	} `json:"machine"`
}

func (m machineTag) MarshalJSON() ([]byte, error) {
	if m.Node == "" {
		return []byte(`""`), nil
	}
	tag := "node " + m.Node
	if m.Machine.HostID != "" {
		tag = "host " + m.Machine.HostID
	}
	model := m.Machine.AcceleratorModel
	if model == "" {
		model = m.Machine.CPUModel
	}
	return json.Marshal(strings.TrimSpace(tag + " " + model))
}

// briefJobs is one compact line per job, the fields of T, ending in job.env: the settings that differ between
// these jobs, always present so that a reader can index it. A job listed alone has nothing to
// differ from, and an empty env there would read as a job that set none, so it shows all of its
// own.
func briefJobs[T any](jobs []json.RawMessage) ([][]byte, error) {
	briefs := make([]T, len(jobs))
	envs := make([]map[string]any, len(jobs))
	for i, raw := range jobs {
		var spec struct {
			Job struct {
				Env map[string]any `json:"env"`
			} `json:"job"`
		}
		if err := json.Unmarshal(raw, &briefs[i]); err != nil {
			return nil, fmt.Errorf("response is not a job: %w", err)
		}
		_ = json.Unmarshal(raw, &spec)
		envs[i] = spec.Job.Env
	}
	lines := make([][]byte, len(jobs))
	for i := range briefs {
		varied := map[string]any{}
		for key, value := range envs[i] {
			if len(envs) == 1 || slices.ContainsFunc(envs, func(other map[string]any) bool { return other[key] != value }) {
				varied[key] = value
			}
		}
		line, _ := json.Marshal(briefs[i])
		line, err := compactJSON(line)
		if err != nil {
			return nil, err
		}
		env, _ := json.Marshal(varied)
		if env, err = compactJSON(env); err != nil {
			return nil, err
		}
		lines[i] = withJobEnv(line, env)
	}
	return lines, nil
}

// withJobEnv adds env to the object line where the platform's API has a job's: under job.env.
// Every hl command that prints a job puts it there, so one script reads them all.
func withJobEnv(line, env []byte) []byte {
	return slices.Concat(line[:len(line)-1], []byte(`,"job":{"env":`), env, []byte("}}"))
}

func jobList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job list", `usage: hl job list [--agent A] [--status S] [--limit N] [--offset N]

One job per line, newest first: ids (hypothesis_id is the hypothesis it tests; a baseline
agent's job tests none and prints "objective":true in its place, so there is nothing to
hl hypothesis get), status, metrics (one value per declared metric, the last the job
reported: its final report on a metric is its result),
ran_on (the machine: numbers compare only between jobs that share it), reasons (for a QUEUED
job not_admitted_reason, the code of what it waits for, and not_admitted_message, its
sentence), theory, cost
and job.env, the settings that differ between the listed jobs (all of them for a job listed
alone). A job's env is under job.env in everything hl prints of a job, as in the platform's
API: here, in hl job status, hl hypothesis get and hl results. hl job status ID
prints a job's whole record. Without --agent, every agent's jobs.`, stderr)
	agent := fs.String("agent", "", "only this agent's jobs; omit to see every agent's")
	pe := fs.String("platform-experiment", "", "platform experiment (default the token's)")
	status := fs.String("status", "", "QUEUED, SUBMITTED, RUNNING, COMPLETED, FAILED, EVICTED, REJECTED or CANCELLED")
	limit := fs.Int("limit", 0, "page size (server default 20, max 50)")
	offset := fs.Int("offset", 0, "jobs to skip")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{}
	if *agent != "" {
		q.Set("agent_id", *agent)
	}
	if p := experimentOf(c, *pe); p != "" {
		q.Set("platform_experiment_id", p)
	}
	if *status != "" {
		q.Set("status", *status)
	}
	if *limit != 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	if *offset != 0 {
		q.Set("offset", strconv.Itoa(*offset))
	}
	var jobs []json.RawMessage
	header, err := c.DoHeader("GET", "/experiments?"+q.Encode(), nil, &jobs)
	if err != nil {
		return fail(stderr, err)
	}
	lines, err := briefJobs[jobBrief](jobs)
	if err != nil {
		return fail(stderr, err)
	}
	for _, line := range lines {
		fmt.Fprintf(stdout, "%s\n", line)
	}
	if len(lines) == 0 {
		fmt.Fprintln(stderr, "hl: job list: no job matches")
	}
	if total, _ := strconv.Atoi(header.Get("X-Total-Count")); total > *offset+len(lines) {
		fmt.Fprintf(stderr, "hl: job list: %d more; --limit N (max 50) and --offset N show them\n", total-*offset-len(lines))
	}
	return 0
}

func jobStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job status", `usage: hl job status ID [ID ...]

One line per job, its whole record: hypothesis_id, or "objective":true on a baseline agent's
job, which tests no hypothesis; status (QUEUED, SUBMITTED, RUNNING, then COMPLETED, FAILED,
EVICTED, REJECTED or CANCELLED), metrics (one value each, as in hl job list), finished_at,
summary, phase_detail, environment_key and the spec under job, its settings as job.env.
eviction_reason says why the last attempt ended, as code or "code: detail": for a FAILED job
how its process ended (container_failed, oom_killed), environment_failed for a REJECTED
one whose environment did not build.
ran_on is where it ran: node, and under machine, for a rented one, instance_id, host_id (the
physical machine, stable across rentals), accelerator_model, cpu_model and price_per_hour_usd.
Two jobs of one spec on different machines are not the same measurement.
A COMPLETED job of an experiment with a constraint metric carries rankable. False means it is
in no standing: not_rankable_reason names the constraint it broke or did not report.
While QUEUED, not_admitted_reason is the code of what the job waits for: awaiting_admission for
the scheduler's first look at it, environment_building for its environment, capacity_unavailable for room on a cluster. not_admitted_message is its
sentence.`, stderr)
	ids, err := parseOperands(fs, args, "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	if len(ids) == 0 {
		return parseExit(misuse(fs, "want at least one ID"))
	}
	code := 0
	for _, id := range ids {
		var out json.RawMessage
		if err := c.Do("GET", "/experiments/"+url.PathEscape(id), nil, &out); err != nil {
			code = fail(stderr, err)
			continue
		}
		if err := printLine(stdout, out, jobDrop...); err != nil {
			return fail(stderr, err)
		}
	}
	return code
}

type metricPoint struct {
	Metric   string  `json:"metric_name"`
	Fraction float64 `json:"fraction_complete"`
	Value    float64 `json:"metric_value"`
	Basis    string  `json:"metric_basis"`
}

type metricSummary struct {
	Metric string  `json:"metric"`
	Basis  string  `json:"basis"`
	Points int     `json:"points"`
	Last   float64 `json:"last"`
	At     float64 `json:"at"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

func jobMetrics(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job metrics", `usage: hl job metrics ID [--points]

One line per reported metric: points reported, the last value and the fraction_complete it was
reported at, and the min and max over the run. --points prints every point instead. Either way
the output is JSON lines, one object per line and no enclosing array: parse it line by line.`, stderr)
	points := fs.Bool("points", false, "print every reported point, oldest first, one JSON object per line")
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+"/metrics", nil, &out); err != nil {
		return fail(stderr, err)
	}
	if *points {
		if err := printLines(stdout, stderr, "job metrics: no metric reported yet", out, "experiment_id"); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	var series []metricPoint
	if err := json.Unmarshal(out, &series); err != nil {
		return fail(stderr, fmt.Errorf("metrics response: %w", err))
	}
	var order []*metricSummary
	byKey := map[[2]string]*metricSummary{}
	for _, p := range series {
		key := [2]string{p.Metric, p.Basis}
		s, ok := byKey[key]
		if !ok {
			s = &metricSummary{Metric: p.Metric, Basis: p.Basis, Min: p.Value, Max: p.Value}
			byKey[key] = s
			order = append(order, s)
		}
		s.Points++
		s.Last, s.At = p.Value, p.Fraction
		s.Min, s.Max = min(s.Min, p.Value), max(s.Max, p.Value)
	}
	for _, s := range order {
		line, _ := json.Marshal(s)
		fmt.Fprintf(stdout, "%s\n", line)
	}
	if len(order) == 0 {
		fmt.Fprintln(stderr, "hl: job metrics: no metric reported yet")
	}
	return 0
}

func jobSummary(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job summary", `usage: hl job summary ID [--text T]

Prints the job's summary. With --text, files the write-up of a finished job when its result
says something: what changed, what the metric did, what it means. It is the job's finding on its hypothesis, shown there and announced
as finding.new, so it needs no comment that repeats it. It is filed once and cannot be replaced
(409 summary_exists): others read it the moment it is announced, so a correction is a comment
on the hypothesis.`, stderr)
	var text string
	fs.StringVar(&text, "text", "", "the summary to file")
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	if text == "" {
		var job struct {
			Summary string `json:"summary"`
		}
		if err := c.Do("GET", "/experiments/"+url.PathEscape(id), nil, &job); err != nil {
			return fail(stderr, err)
		}
		if job.Summary == "" {
			fmt.Fprintf(stderr, "hl: job summary: %s has none filed yet: hl job summary %s --text T files it\n", id, id)
			return 0
		}
		fmt.Fprintln(stdout, job.Summary)
		return 0
	}
	var out struct {
		Status     string `json:"status"`
		Hypothesis string `json:"hypothesis_id"`
		Verdict    string `json:"verdict"`
	}
	if err := c.Do("POST", "/experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": text}, &out); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "{\"status\":%q}\n", out.Status)
	switch out.Verdict {
	case "":
	case "open":
		fmt.Fprintf(stderr, "hl: job summary: the verdict of %s is open and yours to set: hl hypothesis status %s inconclusive, or confirmed|refuted --against CONTROL_JOB once a second run agrees\n", out.Hypothesis, out.Hypothesis)
	default:
		fmt.Fprintf(stderr, "hl: job summary: the verdict of %s is %s and yours to change if this finding does not fit it\n", out.Hypothesis, out.Verdict)
	}
	return 0
}

func jobWait(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job wait", `usage: hl job wait ID [ID ...] [--timeout SECONDS]

Blocks until every named job has ended, then prints each as hl job list does: a FAILED job's
cause and last lines of output are its eviction_reason. Exits 0 once all ended, or 124 at
--timeout with the jobs as they stand.`, stderr)
	timeout := fs.Float64("timeout", 540, "give up after this many `seconds`")
	ids, err := parseOperands(fs, args, "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	if len(ids) == 0 {
		return parseExit(misuse(fs, "want at least one ID"))
	}
	if *timeout <= 0 {
		return parseExit(misuse(fs, "--timeout wants a positive number of seconds"))
	}
	deadline := time.Now().Add(time.Duration(*timeout * float64(time.Second)))
	for _, id := range ids {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		// The watch of one job replays its current status first, so one that ended already
		// returns at once.
		var said bytes.Buffer
		code := watchclient.Run(watchclient.Options{
			URL: c.BaseURL, Token: c.Token, Experiment: id,
			Until:   "status in " + strings.Join(endedStatuses, ","),
			Timeout: left, Stdout: io.Discard, Stderr: &said,
		})
		if code == watchclient.TimeoutExitCode {
			break
		}
		if code != 0 {
			fmt.Fprint(stderr, said.String())
			return code
		}
	}
	jobs := make([]json.RawMessage, 0, len(ids))
	ended := true
	for _, id := range ids {
		var job json.RawMessage
		if err := c.Do("GET", "/experiments/"+url.PathEscape(id), nil, &job); err != nil {
			return fail(stderr, err)
		}
		var status struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(job, &status); err != nil {
			return fail(stderr, fmt.Errorf("response is not a job: %w", err))
		}
		ended = ended && slices.Contains(endedStatuses, status.Status)
		jobs = append(jobs, job)
	}
	lines, err := briefJobs[jobBrief](jobs)
	if err != nil {
		return fail(stderr, err)
	}
	for _, line := range lines {
		fmt.Fprintf(stdout, "%s\n", line)
	}
	if !ended {
		fmt.Fprintf(stderr, "hl: job wait: timed out after %gs with a job not ended\n", *timeout)
		return watchclient.TimeoutExitCode
	}
	return 0
}

func jobLogs(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job logs", "usage: hl job logs ID [--lines N]\n\nThe last log lines the job's cluster reported, oldest first.", stderr)
	n := fs.Int("lines", 50, "how many of the last reported lines")
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	var out []string
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+"/logs?n="+strconv.Itoa(*n), nil, &out); err != nil {
		return fail(stderr, err)
	}
	for _, line := range out {
		fmt.Fprintln(stdout, line)
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "hl: job logs: none reported yet")
	}
	return 0
}

// jobRead prints GET /experiments/{id}<suffix>.
func jobRead(c *apiclient.Client, name, suffix, help string, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job "+name, "usage: hl job "+name+" ID\n\n"+help+".", stderr)
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+suffix, nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "job "+name+": none", out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func jobCancel(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job cancel", "usage: hl job cancel ID\n\nCancels a job that has not ended and prints it, CANCELLED. What it held of your quota is released, down to what it spent running. A job that has ended is refused invalid_transition, with its status.", stderr)
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("POST", "/experiments/"+url.PathEscape(id)+"/cancel", map[string]any{}, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out, jobDrop...); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func jobStats(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job stats", `usage: hl job stats [--agent A] [--hypothesis ID]

Counts over every matching job: by status, capacity tier, eviction reason and eviction class
(workload is yours, infrastructure the environment's, policy the platform's own decision).`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment (default the token's)")
	agent := fs.String("agent", "", "only this agent's jobs")
	hypothesis := fs.String("hypothesis", "", "only jobs testing this hypothesis")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{}
	if p := experimentOf(c, *pe); p != "" {
		q.Set("platform_experiment_id", p)
	}
	if *agent != "" {
		q.Set("agent_id", *agent)
	}
	if *hypothesis != "" {
		id, err := fullHypothesisID(c, q.Get("platform_experiment_id"), *hypothesis)
		if err != nil {
			return fail(stderr, err)
		}
		q.Set("hypothesis_id", id)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/stats?"+q.Encode(), nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}
