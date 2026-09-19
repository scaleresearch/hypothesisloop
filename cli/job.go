package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const jobUsage = `usage: hl job <command> ...

  submit   [FILE|-]            submit a job spec, prints its id; --hypothesis and --env override it
  list                         jobs of your platform experiment, one per line
  status   ID [ID ...]         one line per job: status, queue or eviction reason, spec
  metrics  ID [--points]       per metric: last value, min, max and points reported
  summary  ID --text T         file the write-up of a finished job
  logs     ID [--lines N]      the last log lines its cluster reported
  data     ID                  what the job wrote to the object store
  cost     ID                  what it cost so far, against the estimate
  cancel   ID                  cancel a queued or running job, refunding its credits
  stats                        job counts by status, capacity tier and eviction reason

Run hl job <command> --help for its flags.`

func cmdJob(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, jobUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, jobUsage)
		return 0
	case "submit":
		return jobSubmit(c, rest, stdout, stderr)
	case "list":
		return jobList(c, rest, stdout, stderr)
	case "status":
		return jobStatus(c, rest, stdout, stderr)
	case "metrics":
		return jobMetrics(c, rest, stdout, stderr)
	case "summary":
		return jobSummary(c, rest, stdout, stderr)
	case "logs":
		return jobLogs(c, rest, stdout, stderr)
	case "data":
		return jobRead(c, "data", "/data", "What the job wrote under its data prefix, one object per line", rest, stdout, stderr)
	case "cost":
		return jobRead(c, "cost", "/cost", "Actual consumption so far (settled once final) beside the admission-time estimate", rest, stdout, stderr)
	case "cancel":
		return jobCancel(c, rest, stdout, stderr)
	case "stats":
		return jobStats(c, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hl: job: unknown subcommand %q\n\n%s\n", sub, jobUsage)
		return 2
	}
}

func jobSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job submit", `usage: hl job submit [FILE|-] [--hypothesis ID] [--env K=V ...] [--objective T] [--theory T] [--agent AGENT]

FILE is a YAML file shaped exactly like the POST /experiments body: id, metadata, job. The flags
override it for this one submission, so one committed base spec serves every job that differs
only in its hypothesis and env, with nothing to edit and nothing to commit. Prints the accepted
job's id, status, code_ref, capacity_tier and estimated_cost_acch; hl job status ID has the rest.
An empty id is generated. metadata.hypothesis_id is REQUIRED (from the file or --hypothesis):
every job tests a previously-registered hypothesis (see 'hl hypothesis submit').
metadata.agent_id is set to --agent and an empty metadata.platform_experiment_id to
$PLATFORM_EXPERIMENT_ID. An empty metadata.code_ref is filled with <origin url>@<HEAD sha> of
the git repository in the current directory, which must have no uncommitted changes to tracked
files and HEAD pushed to origin. Commit and git push only after changing a tracked file.
Minimal shape:

  metadata:
    hypothesis_id: hyp-456        # from 'hl hypothesis submit'
    project_id: my-project
    objective: "maximize val_accuracy"
  job:
    image: ...
    accelerator_type: ...
    accelerator_count: 1
    ...

See controlplane/settings/examples/experiment-submission.yaml for the full DSL reference.`, stderr)
	agent := fs.String("agent", "", "stamped onto metadata.agent_id (default $AGENT_ID)")
	overrides := map[string]*string{
		"hypothesis_id": fs.String("hypothesis", "", "sets metadata.hypothesis_id"),
		"objective":     fs.String("objective", "", "sets metadata.objective"),
		"theory":        fs.String("theory", "", "sets metadata.theory"),
	}
	var env stringList
	fs.Var(&env, "env", "job.env entry as `K=V`, repeat per variable")
	path, err := parseOne(fs, args, "FILE", "-")
	if err != nil {
		return parseExit(err)
	}
	*agent = orEnv(*agent, agentEnv)
	if *agent == "" {
		return parseExit(misuse(fs, "--agent is required"))
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
	if v := body["id"]; v == nil || v == "" {
		body["id"] = "job-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	}
	for key, value := range overrides {
		if *value != "" {
			metadata[key] = *value
		}
	}
	if err := setJobEnv(body, env); err != nil {
		return parseExit(misuse(fs, "%s", err))
	}
	metadata["agent_id"] = *agent
	if v := metadata["platform_experiment_id"]; v == nil || v == "" {
		metadata["platform_experiment_id"] = os.Getenv(platformExperimentEnv)
	}
	if err := fillCodeRef(metadata, ""); err != nil {
		return fail(stderr, fmt.Errorf("job submit: %w", err))
	}
	body["metadata"] = metadata
	// The reply repeats the submitted spec; only what the platform decided is printed.
	var out struct {
		ID            string  `json:"id"`
		Status        string  `json:"status,omitempty"`
		CodeRef       string  `json:"code_ref,omitempty"`
		CapacityTier  string  `json:"capacity_tier,omitempty"`
		EstimatedCost float64 `json:"estimated_cost_acch,omitempty"`
		Reason        string  `json:"not_admitted_reason,omitempty"`
	}
	if err := c.Do("POST", "/experiments", body, &out); err != nil {
		return fail(stderr, err)
	}
	held := out.Reason == summaryGate
	out.Reason = ""
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	if !held {
		fmt.Fprintln(stderr, "hl: job submit: "+resultArrives)
		return 0
	}
	q := url.Values{"needs_summary": {"true"}, "agent": {*agent}}
	q.Set("platform_experiment_id", fmt.Sprint(metadata["platform_experiment_id"]))
	var owed []struct {
		ID string `json:"id"`
	}
	if err := c.Do("GET", "/experiments?"+q.Encode(), nil, &owed); err != nil {
		fail(stderr, err)
	}
	commands := make([]string, len(owed))
	for i, job := range owed {
		commands[i] = "hl job summary " + job.ID + " --text T"
	}
	fmt.Fprintln(stderr, "hl: job submit: accepted; it starts once each of your COMPLETED jobs has a summary: "+strings.Join(commands, "; "))
	return 0
}

// summaryGate is the not_admitted_reason of a queued job whose agent owes a summary.
const summaryGate = "summary_gate"

const resultArrives = "the result arrives as an update when you end your turn"

// progress reports whether a job can still change status, and whether an owed summary holds it.
func progress(raw json.RawMessage) (unfinished, held bool) {
	var job struct {
		Status string `json:"status"`
		Reason string `json:"not_admitted_reason"`
	}
	_ = json.Unmarshal(raw, &job)
	switch job.Status {
	case "COMPLETED", "FAILED", "EVICTED", "REJECTED":
		return false, false
	}
	return true, job.Reason == summaryGate
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

// jobDrop leaves out of a printed job what a reader already has: the hypothesis text (hl
// hypothesis get) and the admission-resolved copy of the submitted spec.
var jobDrop = []string{"hypothesis", "resolved_job"}

func jobList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job list", `usage: hl job list [--platform-experiment PE] [--agent A] [--status S] [--needs-summary] [--limit N]

One job per line, in the scheduler's order. Without --agent, every agent's jobs.`, stderr)
	agent := fs.String("agent", "", "only this agent's jobs; omit to see every agent's")
	pe := fs.String("platform-experiment", "", "platform experiment (default $PLATFORM_EXPERIMENT_ID)")
	status := fs.String("status", "", "QUEUED, SUBMITTED, ADMITTED, RUNNING, COMPLETED, FAILED, EVICTED or REJECTED")
	needsSummary := fs.Bool("needs-summary", false, "only COMPLETED jobs still missing a summary, the set that holds your queued jobs back")
	limit := fs.Int("limit", 0, "page size (server default 20, max 200)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{}
	if *agent != "" {
		q.Set("agent", *agent)
	}
	if p := orEnv(*pe, platformExperimentEnv); p != "" {
		q.Set("platform_experiment_id", p)
	}
	if *status != "" {
		q.Set("status", *status)
	}
	if *needsSummary {
		q.Set("needs_summary", "true")
	}
	if *limit != 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments?"+q.Encode(), nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "job list: no job matches", out, jobDrop...); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func jobStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job status", `usage: hl job status ID [ID ...]

One line per job: status (QUEUED, SUBMITTED, RUNNING, then COMPLETED, FAILED, EVICTED or
REJECTED), eviction_reason, phase_detail, and the spec. While QUEUED, not_admitted_reason says
what the job waits for: awaiting_admission for the scheduler's first look at it, summary_gate
for a summary on each of your COMPLETED jobs (hl job list --needs-summary).`, stderr)
	ids, err := parseOperands(fs, args, "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	if len(ids) == 0 {
		return parseExit(misuse(fs, "want at least one ID"))
	}
	code, pending, held := 0, false, false
	for _, id := range ids {
		var out json.RawMessage
		if err := c.Do("GET", "/experiments/"+url.PathEscape(id), nil, &out); err != nil {
			code = fail(stderr, err)
			continue
		}
		if err := printLine(stdout, out, jobDrop...); err != nil {
			return fail(stderr, err)
		}
		unfinished, gated := progress(out)
		pending, held = pending || unfinished, held || gated
	}
	if held {
		fmt.Fprintln(stderr, "hl: job status: summary_gate: it starts once each COMPLETED job of its agent has a summary; hl job list --needs-summary --agent A lists them")
	} else if pending {
		fmt.Fprintln(stderr, "hl: job status: unfinished; "+resultArrives)
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
reported at, and the min and max over the run. --points prints every point instead.`, stderr)
	points := fs.Bool("points", false, "print every reported point, oldest first")
	fs.BoolVar(points, "raw", false, "")
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
	fs := newFlags("job summary", `usage: hl job summary ID --text T

Files the write-up of a finished job: what changed, what the metric did, what it means. While a
COMPLETED job of yours has none, your queued jobs wait.`, stderr)
	var text string
	fs.StringVar(&text, "text", "", "the summary (required)")
	fs.StringVar(&text, "summary", "", "")
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	if text == "" {
		return parseExit(misuse(fs, "--text is required"))
	}
	var out json.RawMessage
	if err := c.Do("POST", "/experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": text}, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
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
	fs := newFlags("job cancel", "usage: hl job cancel ID\n\nCancels a QUEUED or RUNNING job; its credits are refunded.", stderr)
	id, err := parseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("POST", "/experiments/"+url.PathEscape(id)+"/cancel", map[string]any{}, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func jobStats(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job stats", `usage: hl job stats [--platform-experiment PE] [--agent A] [--hypothesis ID]

Counts over every matching job: by status, capacity tier, eviction reason and eviction class
(workload is yours, infrastructure the environment's, policy the platform's own decision).`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment (default $PLATFORM_EXPERIMENT_ID)")
	agent := fs.String("agent", "", "only this agent's jobs")
	hypothesis := fs.String("hypothesis", "", "only jobs testing this hypothesis")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{}
	if p := orEnv(*pe, platformExperimentEnv); p != "" {
		q.Set("platform_experiment_id", p)
	}
	if *agent != "" {
		q.Set("agent", *agent)
	}
	if *hypothesis != "" {
		q.Set("hypothesis_id", *hypothesis)
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
