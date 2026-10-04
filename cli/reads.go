package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

func cmdQuota(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("quota", `usage: hl quota [--agent A | --all] [--platform-experiment PE]

Accelerator-hours granted, used and held, one line per agent. share_accelerator_hours is all the
agent may spend, on guaranteed and burst jobs together. used_guaranteed_acch and used_burst_acch
count a job that has not started at its estimate and a running one at what it has spent.
held_acch is what running jobs hold beyond what they spent: each holds its whole estimate until
it ends. remaining_guaranteed_accelerator_hours and remaining_burst_accelerator_hours are the
largest estimated_cost_acch a job of that tier submitted now is admitted with: the share less
used and held. Burst gives no hours on top of the share: burst_accelerator_hours is how much of
it may run at burst priority, on idle capacity.
In an experiment on CPU cores these are CPU core-hours: a core for an hour costs 1.
A run started with fewer than 5 researchers has one stage, and each holds its whole share
from the start. In a run with more stages a share grows at each stage boundary. While a later
stage is still to come, next_stage_starts_at is when it starts by the clock, earlier once the
same part of the budget is spent, and next_stage_share_accelerator_hours is what
share_accelerator_hours becomes then, at the least. A share that reads spent with these two printed is spent until that time only:
there is more to run after it, and hl platform-experiments agents finish is final. Neither is
printed in the last stage or the only one, before the run starts, or for an agent that was cut.
For one agent, where the platform sets a ceiling: data_bytes is what it holds under its data
prefix and max_data_bytes the ceiling. At or over it every submit is refused with
data_quota_exceeded until some is deleted.
Quota is granted when the platform experiment starts. Before that an agent that holds none
is answered 404 quota_not_granted: hl prints that reason and the platform's word for what the
start waits for and what you do until then, the same as status_reason of
hl platform-experiments get, and exits 1. A draft's planner holds no quota and needs none: it is
answered planner_has_no_quota, printed the same way, with where the run's budget comes from.`, stderr)
	agent := fs.String("agent", "", "agent (default the token's agent)")
	all := fs.Bool("all", false, "every agent signed up to the platform experiment")
	pe := fs.String("platform-experiment", "", "platform experiment (default the token's)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	*pe = experimentOf(c, *pe)
	path := "/platform-experiments/" + url.PathEscape(*pe) + "/quotas"
	if readsEveryAgent(c.Token, *agent) {
		*all = true
	}
	if !*all {
		*agent = agentOf(c, *agent)
		if *agent == "" {
			return parseExit(misuse(fs, "--agent or --all is required"))
		}
		path += "/" + url.PathEscape(*agent)
	}
	if *pe == "" {
		return parseExit(misuse(fs, "--platform-experiment is required"))
	}
	var out json.RawMessage
	if err := c.Do("GET", path, nil, &out); err != nil {
		// An agent's harness tells this refusal from a read that failed by the reason in the line.
		if reason, message := refusalOf(err); reason == "quota_not_granted" || reason == "planner_has_no_quota" {
			fmt.Fprintf(stderr, "hl: quota: %s: %s\n", reason, message)
			return 1
		}
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "quota: none granted yet, it is granted when the platform experiment starts to the agents signed up to it", out, "id", "platform_experiment_id", "created_at"); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func cmdResults(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("results", `usage: hl results [--platform-experiment PE]

The standings on each ranking metric, first the declared baseline's value on it (baseline, with
baseline_job_id: the bar, which moves when the baseline is raised), then one line per agent in
rank order: its best value, beats_baseline (strictly better than that bar) and the job that set it (experiment_id, code_ref, hypothesis_id and its settings under
job.env, where every hl command that prints a job has them) and ran_on, the machine it ran on.
Only COMPLETED jobs that hold every constraint metric rank (rankable in hl job status). At most
one line per agent of the platform experiment and metric, so there is nothing to page or limit.
A metric nobody has a result on yet prints no line, says so on stderr, and exits 0.
Then each reference metric, an outside system the goal compares against and never ranked: one
line per COMPLETED job that reported it, its value, the job, ran_on and constraints_met (false
when the job broke a constraint metric, unknown when it did not report one), or one line saying
it was not measured. A reference job reports the constraint metrics too: only a value whose
constraints_met is true counts. Then a line setting the best of those against the leader on the
first ranking metric when both run in the same direction: beaten false means no result has met
the goal yet, whatever the standings against the control say. Compare a result with a reference
measured on the same machine.`, stderr)
	named := experimentFlag(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	pe, err := namedExperiment(fs, c, *named)
	if err != nil {
		return parseExit(err)
	}
	var out struct {
		Metrics []struct {
			Metric        string            `json:"metric"`
			Direction     string            `json:"direction"`
			Standings     []json.RawMessage `json:"standings"`
			Baseline      *float64          `json:"baseline"`
			BaselineJobID string            `json:"baseline_job_id"`
		} `json:"metrics"`
		References []json.RawMessage `json:"references"`
	}
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(pe)+"/results", nil, &out); err != nil {
		return fail(stderr, err)
	}
	for _, m := range out.Metrics {
		if m.Baseline != nil {
			bar, _ := json.Marshal(map[string]any{"baseline": *m.Baseline, "baseline_job_id": m.BaselineJobID})
			fmt.Fprintf(stdout, "%s\n", prefixed(bar, "metric", m.Metric, "direction", m.Direction))
		}
		for _, s := range m.Standings {
			var standing struct {
				Env map[string]string `json:"env"`
			}
			if err := json.Unmarshal(s, &standing); err != nil {
				return fail(stderr, fmt.Errorf("response is not a standing: %w", err))
			}
			rest, err := compactJSON(s, "env")
			if err != nil {
				return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
			}
			env, _ := json.Marshal(standing.Env)
			if standing.Env == nil {
				env = []byte("{}")
			}
			fmt.Fprintf(stdout, "%s\n", withJobEnv(prefixed(rest, "metric", m.Metric, "direction", m.Direction), env))
		}
		if len(m.Standings) == 0 {
			fmt.Fprintf(stderr, "hl: no results yet on %s\n", m.Metric)
		}
	}
	for _, raw := range out.References {
		var r struct {
			Metric    string            `json:"metric"`
			Direction string            `json:"direction"`
			Values    []json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return fail(stderr, fmt.Errorf("response is not a reference: %w", err))
		}
		for _, v := range r.Values {
			rest, err := compactJSON(v)
			if err != nil {
				return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
			}
			fmt.Fprintf(stdout, "%s\n", prefixed(rest, "reference", r.Metric, "direction", r.Direction))
		}
		if len(r.Values) == 0 {
			fmt.Fprintf(stdout, "%s\n", prefixed(nil, "reference", r.Metric, "direction", r.Direction, "measured", "not measured"))
			continue
		}
		against, err := compactJSON(raw, "metric", "direction", "values")
		if err != nil {
			return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
		}
		fmt.Fprintf(stdout, "%s\n", prefixed(against, "reference", r.Metric, "direction", r.Direction))
	}
	return 0
}

func cmdCapacity(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("capacity", `usage: hl capacity [--org ORG]

Live capacity per cluster, of the clusters your org's jobs can be placed on now: enabled for
the org and reporting. First one line of its cores and memory, in total and not asked for
by running jobs, summed over its nodes, and in per_node the most one node has of each:
cpu_cores_max and memory_bytes_max installed, cpu_cores_available_max and
memory_bytes_available_max free now. A job's cpu, memory and accelerator_count are per node, so
per_node bounds one job: asking for more than the max fits no node and never runs. Then
its priced types, one line per type. For an accelerator: available, total, per_node_available
(the most free on one node now) and acch_per_hour,
what an hour on one of them costs in accelerator hours, and pod_resources where the platform
adds something to every job on the type (the hugepages of a Tenstorrent Blackhole chip): a job
states none of it. For CPU cores (kind cpu, the one type
hypothesisloop.dev/cpu=core): cores_available, cores_total, per_node_cores_available and
acch_per_core_hour, which is 1, so
a budget spent on cores reads as CPU core-hours. A job that needs no accelerator states cpu, a
whole number, and no accelerator_type, and holds that many cores. accelerator_type is the exact
string a job spec takes; a type with none available queues and never errors. Last, one line per
image the deployment offers a job to run in: job_image is its name and what job.image takes,
image its registry address, carries what is installed. A type's line names in job_images the
ones that suit it, so a job there names one of those and installs no framework. job.image is
such a name or the full registry address of an image of your own, host first, and anything
else is refused at submit (unknown_job_image), as is a job no cluster could run with its image
(no_cluster_for_image). Every job names one: a line without only_job_image is of a cluster that runs the
image a job names, and a line with it is of a cluster that runs that image and no other, so a
job meant for it names that image; job_image_carries says what is installed in it. A package a
job needs goes into the environment, .hypothesisloop/Dockerfile, and installs nothing at runtime. A line with
on_demand is of a cluster that rents a machine when a job needs one: nothing stands by, total is
the most it rents at once and available is what it may still rent, so a job there starts after
its machine is rented.

The clusters are the ones one org's jobs can be placed on: its own and the shared ones. An API
key and an agent token have one org and need no --org. A person in several orgs names one.`, stderr)
	org := fs.String("org", "", "org whose clusters to read (default $HL_ORG, then the one org this token is bound to)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	var out struct {
		JobImages []json.RawMessage `json:"job_images"`
		Clusters  []struct {
			Name         string            `json:"cluster_name"`
			JobImage     string            `json:"job_image"`
			Carries      string            `json:"job_image_carries"`
			OnDemand     bool              `json:"on_demand"`
			Resources    json.RawMessage   `json:"resources"`
			PerNode      json.RawMessage   `json:"per_node"`
			Accelerators []json.RawMessage `json:"accelerators"`
		} `json:"clusters"`
	}
	path := "/resource-catalog/capacity"
	if id := orEnv(*org, orgEnv); id != "" {
		path += "?org_id=" + url.QueryEscape(id)
	}
	if err := c.Do("GET", path, nil, &out); err != nil {
		return fail(stderr, err)
	}
	for _, cl := range out.Clusters {
		prefix := []string{"cluster", cl.Name}
		if cl.JobImage != "" {
			prefix = append(prefix, "only_job_image", cl.JobImage)
		}
		if cl.Carries != "" {
			prefix = append(prefix, "job_image_carries", cl.Carries)
		}
		if cl.OnDemand {
			prefix = append(prefix, "on_demand", "true")
		}
		if present(cl.Resources) || present(cl.PerNode) {
			line, err := withPerNode(cl.Resources, cl.PerNode)
			if err != nil {
				return fail(stderr, err)
			}
			if err := printPrefixed(stdout, line, prefix...); err != nil {
				return fail(stderr, err)
			}
		}
		for _, a := range cl.Accelerators {
			a, err := inCores(a)
			if err != nil {
				return fail(stderr, err)
			}
			if err := printPrefixed(stdout, a, prefix...); err != nil {
				return fail(stderr, err)
			}
		}
	}
	for _, image := range out.JobImages {
		line, err := compactJSON(json.RawMessage(strings.Replace(string(image), `"name":`, `"job_image":`, 1)))
		if err != nil {
			return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
		}
		fmt.Fprintln(stdout, string(line))
	}
	if len(out.Clusters) == 0 {
		fmt.Fprintln(stderr, "hl: capacity: no cluster reports capacity")
	}
	return 0
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// withPerNode is a cluster's resources object with its per_node object inside it, so one line
// says both what the cluster has and what one job on it can get.
func withPerNode(resources, perNode json.RawMessage) (json.RawMessage, error) {
	if !present(resources) {
		resources = json.RawMessage("{}")
	}
	line, err := compactJSON(resources)
	if err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	if !present(perNode) {
		return line, nil
	}
	node, err := compactJSON(perNode)
	if err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	var b bytes.Buffer
	b.Write(line[:len(line)-1])
	if len(line) > 2 {
		b.WriteByte(',')
	}
	b.WriteString(`"per_node":`)
	b.Write(node)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// inCores renames the counts and the rate of a capacity line of kind cpu, so that the line says
// they are cores. A line of any other kind is returned as it came.
func inCores(raw json.RawMessage) (json.RawMessage, error) {
	var line struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	if line.Kind != "cpu" {
		return raw, nil
	}
	out := string(raw)
	for from, to := range map[string]string{`"available":`: `"cores_available":`, `"total":`: `"cores_total":`, `"acch_per_hour":`: `"acch_per_core_hour":`, `"per_node_available":`: `"per_node_cores_available":`} {
		out = strings.Replace(out, from, to, 1)
	}
	return json.RawMessage(out), nil
}

// printPrefixed prints the object raw on one line after the given key and value pairs.
func printPrefixed(stdout io.Writer, raw json.RawMessage, kv ...string) error {
	rest, err := compactJSON(raw)
	if err != nil {
		return fmt.Errorf("response is not JSON: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "%s\n", prefixed(rest, kv...))
	return err
}

// prefixed is the compact object rest with the given key and value pairs ahead of its own.
func prefixed(rest []byte, kv ...string) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		writeString(&b, kv[i])
		b.WriteByte(':')
		writeString(&b, kv[i+1])
	}
	if len(rest) > 2 {
		b.WriteByte(',')
		b.Write(rest[1:])
	} else {
		b.WriteByte('}')
	}
	return b.Bytes()
}
