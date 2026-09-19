package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const jobUsage = `usage: hl job <command> ...

  submit   [FILE|-]            submit a job spec, prints the accepted job
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
		return jobRead(c, "data", "/data", "what the job wrote under its data prefix, one object per line", rest, stdout, stderr)
	case "cost":
		return jobRead(c, "cost", "/cost", "actual consumption so far (settled once final) beside the admission-time estimate", rest, stdout, stderr)
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
	fs := newFlags("job submit", `usage: hl job submit [--agent AGENT] [FILE|-]

FILE is a YAML file shaped exactly like the POST /experiments body: id, metadata, job.
metadata.hypothesis_id is REQUIRED: every job tests a specific, previously-registered
hypothesis (see 'hl hypothesis submit'), there is no free-text/no-hypothesis path. An empty
metadata.code_ref is filled with <origin url>@<HEAD sha> of the git repository in the current
directory; that repository must have no uncommitted changes to tracked files and HEAD pushed to
origin. Minimal shape:

  id: job-123
  metadata:
    platform_experiment_id: pe-123
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
	metadata["agent_id"] = *agent
	if err := fillCodeRef(metadata, ""); err != nil {
		return fail(stderr, fmt.Errorf("job submit: %w", err))
	}
	body["metadata"] = metadata
	var out any
	if err := c.Do("POST", "/experiments", body, &out); err != nil {
		return fail(stderr, err)
	}
	printJSON(stdout, out)
	return 0
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
	needsSummary := fs.Bool("needs-summary", false, "only finished jobs still missing a summary, the set that blocks your next submit")
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
	if err := printLines(stdout, out, jobDrop...); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func jobStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job status", `usage: hl job status ID [ID ...]

One line per job: status (QUEUED, SUBMITTED, RUNNING, then COMPLETED, FAILED, EVICTED or
REJECTED), not_admitted_reason while queued, eviction_reason, phase_detail, and the spec.`, stderr)
	ids, err := parseOperands(fs, args, "id")
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
reported at, and the min and max over the run. --points prints every point instead.`, stderr)
	points := fs.Bool("points", false, "print every reported point, oldest first")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+"/metrics", nil, &out); err != nil {
		return fail(stderr, err)
	}
	if *points {
		if err := printLines(stdout, out, "experiment_id"); err != nil {
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

Files the write-up of a finished job: what changed, what the metric did, what it means. Every
COMPLETED job needs one before your next submit.`, stderr)
	var text string
	fs.StringVar(&text, "text", "", "the summary (required)")
	fs.StringVar(&text, "summary", "", "")
	id, err := parseOne(fs, args, "ID", "", "id")
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
	id, err := parseOne(fs, args, "ID", "", "id")
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
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+suffix, nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func jobCancel(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("job cancel", "usage: hl job cancel ID\n\nCancels a QUEUED or RUNNING job; its credits are refunded.", stderr)
	id, err := parseOne(fs, args, "ID", "", "id")
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
