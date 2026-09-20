package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

var jobCommands = []subcommand{
	{"submit", "submit a job spec, prints its id", jobSubmit},
	{"list", "jobs, newest first, one brief line each with its metrics", jobList},
	{"status", "a job's whole record", jobStatus},
	{"metrics", "per metric: last value, min, max and points reported", jobMetrics},
	{"summary", "file the write-up of a finished job", jobSummary},
	{"logs", "the last log lines its cluster reported", jobLogs},
	{"data", "what the job wrote to the object store", func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return jobRead(c, "data", "/data", "What the job wrote under its data prefix, one object per line", args, stdout, stderr)
	}},
	{"cost", "what it cost so far, against the estimate", func(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
		return jobRead(c, "cost", "/cost", "Actual consumption so far (settled once final) beside the admission-time estimate", args, stdout, stderr)
	}},
	{"cancel", "cancel a queued or running job, refunding its credits", jobCancel},
	{"stats", "job counts by status, capacity tier and eviction reason", jobStats},
}

func jobSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job submit", `usage: hl job submit [FILE|-] [--hypothesis ID] [--env K=V ...] [--objective T] [--theory T]

FILE is a YAML job spec: id, metadata, job. The flags override it for this one submission, so
one committed base spec serves every job that differs only in its hypothesis and env, with
nothing to edit and nothing to commit. Prints the accepted job's id, status, code_ref,
capacity_tier and estimated_cost_acch; hl job status ID has the rest.
An empty id is generated. metadata.hypothesis_id is REQUIRED (from the file or --hypothesis):
every job tests a previously-registered hypothesis (see 'hl hypothesis submit').
metadata.agent_id is set to --agent and an empty metadata.platform_experiment_id to
--platform-experiment. An empty metadata.code_ref is filled with <origin url>@<HEAD sha> of
the git repository in the current directory, which must have no uncommitted changes to tracked
files and HEAD pushed to origin. Commit and git push only after changing a tracked file.
Minimal shape:

  metadata:
    hypothesis_id: hyp-456        # from 'hl hypothesis submit'
    objective: "maximize val_accuracy"
  job:
    image: ...
    accelerator_type: ...
    accelerator_count: 1
    ...

See controlplane/settings/examples/experiment-submission.yaml for the full DSL reference.`, stderr)
	agent := fs.String("agent", "", "stamped onto metadata.agent_id (default $AGENT_ID)")
	overrides := map[string]*string{
		"hypothesis_id":          fs.String("hypothesis", "", "sets metadata.hypothesis_id"),
		"objective":              fs.String("objective", "", "sets metadata.objective"),
		"theory":                 fs.String("theory", "", "sets metadata.theory"),
		"platform_experiment_id": fs.String("platform-experiment", "", "sets metadata.platform_experiment_id (default the file's, then $PLATFORM_EXPERIMENT_ID)"),
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
		var refused *apiclient.HTTPError
		if errors.As(err, &refused) && strings.Contains(refused.Body, `"experiment_not_started"`) {
			fmt.Fprintln(stderr, "hl: job submit: "+notStarted(""))
			return 1
		}
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
	// The job is accepted whatever this read does: a failed one must not print as an error.
	commands := []string{"hl job list --needs-summary --agent " + *agent + " lists them"}
	if err := c.Do("GET", "/experiments?"+q.Encode(), nil, &owed); err == nil && len(owed) > 0 {
		commands = commands[:0]
		for _, job := range owed {
			commands = append(commands, "hl job summary "+job.ID+" --text T")
		}
	}
	fmt.Fprintln(stderr, "hl: job submit: accepted; it starts once each of your COMPLETED jobs has a summary: "+strings.Join(commands, "; "))
	return 0
}

// summaryGate is the not_admitted_reason of a queued job whose agent owes a summary.
const summaryGate = "summary_gate"

const resultArrives = "the result arrives as an update with its metrics when you end your turn"

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
// hypothesis get), the admission-resolved copy of the submitted spec and the ranking metric, which
// metrics repeats.
var jobDrop = []string{"hypothesis", "resolved_job", "metric"}

// jobBrief is what a listing prints of a job; hl job status ID prints the whole record.
type jobBrief struct {
	ID                string          `json:"id"`
	ParentID          string          `json:"parent_id"`
	AgentID           string          `json:"agent_id"`
	HypothesisID      string          `json:"hypothesis_id"`
	Status            string          `json:"status"`
	Metrics           briefMetrics    `json:"metrics"`
	NotAdmittedReason string          `json:"not_admitted_reason"`
	EvictionReason    string          `json:"eviction_reason"`
	PhaseDetail       json.RawMessage `json:"phase_detail"`
	Theory            string          `json:"theory"`
	EstimatedCost     float64         `json:"estimated_cost_acch"`
	CreatedAt         string          `json:"created_at"`
}

// briefMetrics prints a job's metrics as name: best and last, at six significant digits, with
// the basis when it is not raw.
type briefMetrics []struct {
	Name  string  `json:"name"`
	Best  float64 `json:"best"`
	Last  float64 `json:"last"`
	Basis string  `json:"basis"`
}

func (m briefMetrics) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, metric := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%q:{"best":%s,"last":%s`, metric.Name,
			strconv.FormatFloat(metric.Best, 'g', 6, 64), strconv.FormatFloat(metric.Last, 'g', 6, 64))
		if metric.Basis != "raw" {
			fmt.Fprintf(&b, `,"basis":%q`, metric.Basis)
		}
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// briefJobs is one compact line per job, ending in env: the settings that differ between these
// jobs, always present so that a reader can index it.
func briefJobs(jobs []json.RawMessage) ([][]byte, error) {
	briefs := make([]jobBrief, len(jobs))
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
			if slices.ContainsFunc(envs, func(other map[string]any) bool { return other[key] != value }) {
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
		lines[i] = slices.Concat(line[:len(line)-1], []byte(`,"env":`), env, []byte("}"))
	}
	return lines, nil
}

func jobList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job list", `usage: hl job list [--agent A] [--status S] [--needs-summary] [--limit N] [--offset N]

One job per line, newest first: ids, status, metrics (best and last of each declared metric),
reasons, theory, cost and env, the settings that differ between the listed jobs. hl job status ID
prints a job's whole record. Without --agent, every agent's jobs.`, stderr)
	agent := fs.String("agent", "", "only this agent's jobs; omit to see every agent's")
	pe := fs.String("platform-experiment", "", "platform experiment (default $PLATFORM_EXPERIMENT_ID)")
	status := fs.String("status", "", "QUEUED, SUBMITTED, RUNNING, COMPLETED, FAILED, EVICTED or REJECTED")
	needsSummary := fs.Bool("needs-summary", false, "only COMPLETED jobs still missing a summary, the set that holds your queued jobs back")
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
	if *offset != 0 {
		q.Set("offset", strconv.Itoa(*offset))
	}
	var jobs []json.RawMessage
	header, err := c.DoHeader("GET", "/experiments?"+q.Encode(), nil, &jobs)
	if err != nil {
		return fail(stderr, err)
	}
	lines, err := briefJobs(jobs)
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

One line per job: status (QUEUED, SUBMITTED, RUNNING, then COMPLETED, FAILED, EVICTED or
REJECTED), metrics, finished_at, summary, eviction_reason, phase_detail, and the spec. While QUEUED, not_admitted_reason says
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
		fmt.Fprintf(stderr, "hl: job summary: the verdict of %s is open and yours to set: hl hypothesis status %s confirmed|refuted|inconclusive\n", out.Hypothesis, out.Hypothesis)
	default:
		fmt.Fprintf(stderr, "hl: job summary: the verdict of %s is %s and yours to change if this finding does not fit it\n", out.Hypothesis, out.Verdict)
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
	fs := newFlags("job stats", `usage: hl job stats [--agent A] [--hypothesis ID]

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
