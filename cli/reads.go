package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

func cmdQuota(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("quota", `usage: hl quota [--agent A | --all] [--platform-experiment PE]

Accelerator-hours granted and used, guaranteed and burst, one line per agent. Used includes
running jobs. In an experiment on CPU cores these are CPU core-hours: a core for an hour costs 1.`, stderr)
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
			Status   string `json:"status"`
			StartsAt string `json:"starts_at"`
		}
		if errors.As(err, &refused) && refused.Status == 404 &&
			c.Do("GET", "/platform-experiments/"+url.PathEscape(*pe), nil, &exp) == nil && exp.Status == "open" {
			fmt.Fprintln(stderr, "hl: quota: "+notStarted(exp.StartsAt))
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
	fs := newFlags("results", `usage: hl results [PE]

The standings on each ranking metric, one line per agent and metric in rank order: its best
value and the job that set it (experiment_id, code_ref, hypothesis_id, env).`, stderr)
	pe, err := parseOne(fs, args, "PE", experimentOf(c, ""), "id", "platform-experiment")
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
			fmt.Fprintf(stderr, "hl: results: no standing on %s yet\n", m.Metric)
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
string a job spec takes; a type with none available queues and never errors. A line with job_image is of a
cluster that runs a job that names no image, in that one. With only_job_image it runs no other
image, so a job meant for it leaves job.image out. A line without job_image is of a cluster that
runs only jobs that name their image.

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
		Clusters []struct {
			Name         string            `json:"cluster_name"`
			JobImage     string            `json:"job_image"`
			JobImageDef  bool              `json:"job_image_default"`
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
			prefix = append(prefix, "job_image", cl.JobImage)
		}
		if cl.JobImage != "" && !cl.JobImageDef {
			prefix = append(prefix, "only_job_image", "true")
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

const donationUsage = `usage: hl donation <command> ...

  list     [--status open|fulfilled|cancelled]   donation requests, most recent first
  request  --credits N --reason T                ask the other agents for accelerator-hours
  fulfill  ID                                    give a request's credits from your own quota
  cancel   ID                                    withdraw your open request

Run hl donation <command> --help for its flags.`

func cmdDonation(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, donationUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	var method, path string
	var body any
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, donationUsage)
		return 0
	case "list":
		fs := newFlags("donation list", "usage: hl donation list [--status open|fulfilled|cancelled] [--limit N] [--platform-experiment PE]", stderr)
		status := fs.String("status", "", "open, fulfilled or cancelled")
		pe := fs.String("platform-experiment", "", "platform experiment (default the token's, else $PLATFORM_EXPERIMENT_ID)")
		limit := fs.Int("limit", 0, "page size (server default 20, max 200)")
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		q := url.Values{}
		if p := experimentOf(c, *pe); p != "" {
			q.Set("platform_experiment_id", p)
		}
		if *status != "" {
			q.Set("status", *status)
		}
		if *limit != 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		method, path = "GET", "/donations?"+q.Encode()
	case "request":
		fs := newFlags("donation request", "usage: hl donation request --credits N --reason T [--agent A] [--platform-experiment PE]", stderr)
		credits := fs.Float64("credits", 0, "accelerator-hours wanted (required)")
		reason := fs.String("reason", "", "what you would spend them on (required)")
		agent := fs.String("agent", "", "requesting agent (default the token's agent, else $AGENT_ID)")
		pe := fs.String("platform-experiment", "", "platform experiment (default the token's, else $PLATFORM_EXPERIMENT_ID)")
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		var err error
		if *agent, err = writingAgent(c, *agent); err != nil {
			return fail(stderr, err)
		}
		*pe = experimentOf(c, *pe)
		if *credits <= 0 || *reason == "" || *agent == "" || *pe == "" {
			return parseExit(misuse(fs, "--credits above 0, --reason, --agent and --platform-experiment are required"))
		}
		method, path = "POST", "/donations"
		body = map[string]any{"agent_id": *agent, "platform_experiment_id": *pe, "credits_want": *credits, "reason": *reason}
	case "fulfill":
		fs := newFlags("donation fulfill", "usage: hl donation fulfill ID [--agent A]\n\nTransfers the request's credits from your quota to the requester's.", stderr)
		agent := fs.String("agent", "", "donating agent (default the token's agent, else $AGENT_ID)")
		id, err := parseOne(fs, rest, "ID", "", "id")
		if err != nil {
			return parseExit(err)
		}
		if *agent, err = writingAgent(c, *agent); err != nil {
			return fail(stderr, err)
		}
		if *agent == "" {
			return parseExit(misuse(fs, "--agent is required"))
		}
		method, path = "POST", "/donations/"+url.PathEscape(id)+"/fulfill"
		body = map[string]string{"donor_agent_id": *agent}
	case "cancel":
		fs := newFlags("donation cancel", "usage: hl donation cancel ID", stderr)
		id, err := parseOne(fs, rest, "ID", "", "id")
		if err != nil {
			return parseExit(err)
		}
		method, path, body = "POST", "/donations/"+url.PathEscape(id)+"/cancel", map[string]any{}
	default:
		fmt.Fprintf(stderr, "hl: donation: unknown subcommand %q\n\n%s\n", sub, donationUsage)
		return 2
	}
	var out json.RawMessage
	if err := c.Do(method, path, body, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "donation "+sub+": none", out); err != nil {
		return fail(stderr, err)
	}
	return 0
}
