package shared

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/watchclient"
	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

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
	Image              string          `json:"image"`
	PhaseDetail        json.RawMessage `json:"phase_detail"`
	Theory             string          `json:"theory"`
	EstimatedCost      float64         `json:"estimated_cost_acch"`
	Rankable           *bool           `json:"rankable,omitempty"`
	NotRankableReason  string          `json:"not_rankable_reason,omitempty"`
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
		if metric.Basis != platformapi.BasisRaw {
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
	Node    string                 `json:"node"`
	Machine *clusterapi.JobMachine `json:"machine"`
}

func (m machineTag) MarshalJSON() ([]byte, error) {
	if m.Node == "" {
		return []byte(`""`), nil
	}
	tag, model := "node "+m.Node, ""
	if m.Machine != nil {
		if m.Machine.HostID != "" {
			tag = "host " + m.Machine.HostID
		}
		model = cmp.Or(m.Machine.AcceleratorModel, m.Machine.CPUModel)
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
		line, err := command.CompactJSON(line)
		if err != nil {
			return nil, err
		}
		env, _ := json.Marshal(varied)
		if env, err = command.CompactJSON(env); err != nil {
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

func JobList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job list", `usage: hl job list [--agent A] [--status S] [--limit N] [--offset N]

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
	offset := fs.Int("offset", 0, "rows to skip")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{}
	if *agent != "" {
		q.Set("agent_id", *agent)
	}
	if p := c.Experiment(*pe); p != "" {
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
		return command.Fail(stderr, err)
	}
	lines, err := briefJobs[jobBrief](jobs)
	if err != nil {
		return command.Fail(stderr, err)
	}
	for _, line := range lines {
		fmt.Fprintf(stdout, "%s\n", line)
	}
	if len(lines) == 0 {
		fmt.Fprintln(stderr, "hl: job list: no job matches")
	}
	command.PageNotice(stderr, "job list", header, *offset, len(lines))
	return 0
}

func JobStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job status", `usage: hl job status ID [ID ...]

One line per job, its whole record: hypothesis_id, or "objective":true on a baseline agent's
job, which tests no hypothesis; status (QUEUED, SUBMITTED, RUNNING, then COMPLETED, FAILED,
EVICTED, REJECTED or CANCELLED), metrics (one value each, as in hl job list), finished_at,
summary, phase_detail, image (repository@digest) and the spec under job, its settings as job.env.
eviction_reason says why the last attempt ended, as code or "code: detail": for a FAILED job
how its process ended (container_failed, oom_killed).
ran_on is where it ran: node, and under machine, for a rented one, instance_id, host_id (the
physical machine, stable across rentals), accelerator_model, cpu_model and price_per_hour_usd.
Two jobs of one spec on different machines are not the same measurement.
A COMPLETED job of an experiment with a constraint metric carries rankable. False means it is
in no standing: not_rankable_reason names the constraint it broke or did not report.
While QUEUED, not_admitted_reason is the code of what the job waits for: awaiting_admission for
the scheduler's first look at it, capacity_unavailable for room on a cluster. not_admitted_message is its
sentence.`, stderr)
	ids, err := command.ParseOperands(fs, args, "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	if len(ids) == 0 {
		return command.ParseExit(command.Misuse(fs, "want at least one ID"))
	}
	code := 0
	for _, id := range ids {
		var out json.RawMessage
		if err := c.Do("GET", "/experiments/"+url.PathEscape(id), nil, &out); err != nil {
			code = command.Fail(stderr, err)
			continue
		}
		if err := command.PrintLine(stdout, out, jobDrop...); err != nil {
			return command.Fail(stderr, err)
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

func JobMetrics(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job metrics", `usage: hl job metrics ID [--points]

One line per reported metric: points reported, the last value and the fraction_complete it was
reported at, and the min and max over the run. --points prints every point instead. Either way
the output is JSON lines, one object per line and no enclosing array: parse it line by line.`, stderr)
	points := fs.Bool("points", false, "print every reported point, oldest first, one JSON object per line")
	id, err := command.ParseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+"/metrics", nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if *points {
		if err := command.PrintLines(stdout, stderr, "job metrics: no metric reported yet", out, "experiment_id"); err != nil {
			return command.Fail(stderr, err)
		}
		return 0
	}
	var series []metricPoint
	if err := json.Unmarshal(out, &series); err != nil {
		return command.Fail(stderr, fmt.Errorf("metrics response: %w", err))
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

func JobSummary(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job summary", `usage: hl job summary ID [--text T | --text-file FILE|-]

Prints the job's summary. With --text, or --text-file reading it from FILE or stdin for -,
files the write-up of a finished job when its result says something: what changed, what the metric did, what it means. It is the job's finding on its hypothesis, shown there and announced
as finding.new, so it needs no comment that repeats it. It is filed once and cannot be replaced
(409 summary_exists): others read it the moment it is announced, so a correction is a comment
on the hypothesis.`, stderr)
	summary := command.NewTextFlag(fs, "text", "the summary to file")
	id, err := command.ParseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	text, err := summary.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if text == "" {
		var job struct {
			Summary string `json:"summary"`
		}
		if err := c.Do("GET", "/experiments/"+url.PathEscape(id), nil, &job); err != nil {
			return command.Fail(stderr, err)
		}
		if job.Summary == "" {
			fmt.Fprintf(stderr, "hl: job summary: %s has none filed yet: hl job summary %s --text T files it\n", id, id)
			return 0
		}
		fmt.Fprintln(stdout, job.Summary)
		return 0
	}
	var out struct {
		Status     string                       `json:"status"`
		Hypothesis string                       `json:"hypothesis_id"`
		Verdict    platformapi.HypothesisStatus `json:"verdict"`
	}
	if err := c.Do("POST", "/experiments/"+url.PathEscape(id)+"/summary", map[string]string{"summary": text}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintf(stdout, "{\"status\":%q}\n", out.Status)
	switch out.Verdict {
	case "":
	case platformapi.HypothesisOpen:
		fmt.Fprintf(stderr, "hl: job summary: the verdict of %s is open and yours to set: hl hypothesis status %s inconclusive, or confirmed|refuted --against CONTROL_JOB once a second run agrees (one run suffices where every ranking metric is deterministic)\n", out.Hypothesis, out.Hypothesis)
	default:
		fmt.Fprintf(stderr, "hl: job summary: the verdict of %s is %s and yours to change if this finding does not fit it\n", out.Hypothesis, out.Verdict)
	}
	return 0
}

func JobWait(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job wait", `usage: hl job wait ID [ID ...] [--timeout SECONDS]

Blocks until every named job has ended, then prints each as hl job list does: a FAILED job's
cause and last lines of output are its eviction_reason. Exits 0 once all ended, or 124 at
--timeout with the jobs as they stand.`, stderr)
	timeout := fs.Float64("timeout", 540, "give up after this many `seconds`")
	ids, err := command.ParseOperands(fs, args, "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	if len(ids) == 0 {
		return command.ParseExit(command.Misuse(fs, "want at least one ID"))
	}
	if *timeout <= 0 {
		return command.ParseExit(command.Misuse(fs, "--timeout wants a positive number of seconds"))
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
			Until:   "status in " + joined(platformapi.EndedStatuses, ","),
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
			return command.Fail(stderr, err)
		}
		var status struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(job, &status); err != nil {
			return command.Fail(stderr, fmt.Errorf("response is not a job: %w", err))
		}
		ended = ended && platformapi.ExperimentStatus(status.Status).Ended()
		jobs = append(jobs, job)
	}
	lines, err := briefJobs[jobBrief](jobs)
	if err != nil {
		return command.Fail(stderr, err)
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

func JobLogs(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job logs", `usage: hl job logs ID [--lines N]

The last log lines the job's cluster reported. A job of one node prints them oldest first. A job
of several nodes prints a section per rank whose pod or instance still exists, each oldest first
under a header that names its rank, group and pod, "--- rank 2 failed, exit 137 (group actor,
pod P) ---" for a rank that failed: the other ranks by rank, then rank 0, then the failed ranks,
the first to fail at the very end. Once a rank fails the others' pods are removed, so the tail
may hold the failed rank alone. The ranks share --lines, and when not all fit the failed ranks and rank 0 are kept,
under a first line "--- N more ranks not shown ---".`, stderr)
	n := fs.Int("lines", 50, "how many of the last reported lines")
	id, err := command.ParseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	var out []string
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+"/logs?n="+strconv.Itoa(*n), nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	for _, line := range out {
		fmt.Fprintln(stdout, line)
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "hl: job logs: none reported yet")
	}
	return 0
}

func JobData(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	return jobRead(c, "data", "/data", "What the job wrote under its data prefix, one object per line, each with a url a plain GET reads it by for 12 hours: how a job of a prior run is read", args, stdout, stderr)
}

func JobCost(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	return jobRead(c, "cost", "/cost", "Actual consumption so far (settled once final) beside the admission-time estimate", args, stdout, stderr)
}

// jobRead prints GET /experiments/{id}<suffix>.
func jobRead(c *apiclient.Client, name, suffix, help string, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job "+name, "usage: hl job "+name+" ID\n\n"+help+".", stderr)
	id, err := command.ParseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/"+url.PathEscape(id)+suffix, nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLines(stdout, stderr, "job "+name+": none", out); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

func JobCancel(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job cancel", "usage: hl job cancel ID\n\nCancels a job that has not ended and prints it, CANCELLED. What it held of your quota is released, down to what it spent running. A job that has ended is refused invalid_transition, with its status.", stderr)
	id, err := command.ParseOne(fs, args, "ID", "", "id", "experiment")
	if err != nil {
		return command.ParseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("POST", "/experiments/"+url.PathEscape(id)+"/cancel", map[string]any{}, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLine(stdout, out, jobDrop...); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

func JobStats(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("job stats", `usage: hl job stats [--agent A] [--hypothesis ID]

Counts over every matching job: by status, capacity tier, eviction reason and eviction class
(workload is yours, infrastructure the environment's, policy the platform's own decision).`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment (default the token's)")
	agent := fs.String("agent", "", "only this agent's jobs")
	hypothesis := fs.String("hypothesis", "", "only jobs testing this hypothesis")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	q := url.Values{}
	if p := c.Experiment(*pe); p != "" {
		q.Set("platform_experiment_id", p)
	}
	if *agent != "" {
		q.Set("agent_id", *agent)
	}
	if *hypothesis != "" {
		id, err := apiclient.FullHypothesisID(c, q.Get("platform_experiment_id"), *hypothesis)
		if err != nil {
			return command.Fail(stderr, err)
		}
		q.Set("hypothesis_id", id)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/experiments/stats?"+q.Encode(), nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLine(stdout, out); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}
