package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

func cmdQuota(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("quota", `usage: hl quota [--agent A | --all] [--platform-experiment PE]

Accelerator-hours granted and used, one line per agent. share_accelerator_hours is all the
agent may spend, on guaranteed and burst jobs together. remaining_guaranteed_accelerator_hours
and remaining_burst_accelerator_hours are the largest estimated_cost_acch a job of that tier
submitted now is admitted with: every job that has not ended holds its whole estimate, so they
are less than the share minus what is used while jobs run. Burst gives no hours on top of the
share: burst_accelerator_hours is how much of it may run at burst priority, on idle capacity.
Used counts a job that has not started at its estimate and a running one at what it has spent.
In an experiment on CPU cores these are CPU core-hours: a core for an hour costs 1.
For one agent, where the platform sets a ceiling: data_bytes is what it holds under its data
prefix and max_data_bytes the ceiling. At or over it every submit is refused with
data_quota_exceeded until some is deleted.`, stderr)
	agent := fs.String("agent", "", "agent (default the token's agent, else $AGENT_ID)")
	all := fs.Bool("all", false, "every agent signed up to the platform experiment")
	pe := fs.String("platform-experiment", "", "platform experiment (default the token's, else $PLATFORM_EXPERIMENT_ID)")
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
		// Quota is granted at the start, so before it the agent has none: no error of its own.
		var refused *apiclient.HTTPError
		var exp struct {
			Status      string `json:"status"`
			BaselineJob string `json:"baseline_job_id"`
		}
		if errors.As(err, &refused) && refused.Status == 404 &&
			c.Do("GET", "/platform-experiments/"+url.PathEscape(*pe), nil, &exp) == nil && exp.Status == "open" {
			notice, err := openNotice(c, *pe, *agent, exp.BaselineJob)
			if err != nil {
				return fail(stderr, err)
			}
			fmt.Fprintln(stderr, "hl: quota: "+notice)
			return 0
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

The standings on each ranking metric, one line per agent and metric in rank order: its best
value and the job that set it (experiment_id, code_ref, hypothesis_id, env). Only COMPLETED jobs
that hold every constraint metric rank (rankable in hl job status). At most one line
per agent of the platform experiment and metric, so there is nothing to page or limit. A metric
nobody has a result on yet prints no line, says so on stderr, and exits 0.`, stderr)
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
			Metric    string            `json:"metric"`
			Direction string            `json:"direction"`
			Standings []json.RawMessage `json:"standings"`
		} `json:"metrics"`
	}
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(pe)+"/results", nil, &out); err != nil {
		return fail(stderr, err)
	}
	for _, m := range out.Metrics {
		for _, s := range m.Standings {
			if err := printPrefixed(stdout, s, "metric", m.Metric, "direction", m.Direction); err != nil {
				return fail(stderr, err)
			}
		}
		if len(m.Standings) == 0 {
			fmt.Fprintf(stderr, "hl: no results yet on %s\n", m.Metric)
		}
	}
	return 0
}

func cmdCapacity(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("capacity", `usage: hl capacity [--org ORG]

Live capacity per cluster. First one line of its cores and memory, in total and not asked for
by running jobs, summed over its nodes: what a job with no accelerator needs to know. Then
its priced types, one line per type. For an accelerator: available, total and acch_per_hour,
what an hour on one of them costs in accelerator hours. For CPU cores (kind cpu, the one type
hypothesisloop.dev/cpu=core): cores_available, cores_total and acch_per_core_hour, which is 1, so
a budget spent on cores reads as CPU core-hours. A job that needs no accelerator states cpu, a
whole number, and no accelerator_type, and holds that many cores. accelerator_type is the exact
string a job spec takes; a type with none available queues and never errors. Last, one line per
image the deployment offers a job to run in: job_image its name, image what job.image takes,
carries what is installed. A type's line names in job_images the ones that suit it, so a job
there names one of those and installs no framework. A cluster's line with only_job_image is of
a cluster that runs that image and no other, so a job meant for it leaves job.image out, and
job_image_carries says what is installed in it. A line without only_job_image is of a cluster
that runs the image a job names, and a job that names none is not placed there. A line with
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
		if len(cl.Resources) > 0 && string(cl.Resources) != "null" {
			if err := printPrefixed(stdout, cl.Resources, prefix...); err != nil {
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
	for from, to := range map[string]string{`"available":`: `"cores_available":`, `"total":`: `"cores_total":`, `"acch_per_hour":`: `"acch_per_core_hour":`} {
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
	_, err = fmt.Fprintf(stdout, "%s\n", b.Bytes())
	return err
}
