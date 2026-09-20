package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

func cmdQuota(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("quota", `usage: hl quota [--agent A | --all] [--platform-experiment PE]

Accelerator-hours granted and used, guaranteed and burst, one line per agent. Used includes
running jobs.`, stderr)
	agent := fs.String("agent", "", "agent (default $AGENT_ID)")
	all := fs.Bool("all", false, "every agent signed up to the platform experiment")
	pe := fs.String("platform-experiment", "", "platform experiment (default $PLATFORM_EXPERIMENT_ID)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	*pe = orEnv(*pe, platformExperimentEnv)
	path := "/platform-experiments/" + url.PathEscape(*pe) + "/quotas"
	if !*all {
		*agent = orEnv(*agent, agentEnv)
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
	pe, err := parseOne(fs, args, "PE", os.Getenv(platformExperimentEnv), "id", "platform-experiment")
	if err != nil {
		return parseExit(err)
	}
	var out struct {
		Status  string `json:"status"`
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
	fs := newFlags("capacity", `usage: hl capacity

Live accelerators per cluster, one line per type: available, total and acch_per_hour, what an
hour on one of them costs in accelerator hours. accelerator_type is the exact string a job spec
takes; a type with none available queues and never errors.`, stderr)
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	var out struct {
		Clusters []struct {
			Name         string            `json:"cluster_name"`
			Accelerators []json.RawMessage `json:"accelerators"`
		} `json:"clusters"`
	}
	if err := c.Do("GET", "/resource-catalog/capacity", nil, &out); err != nil {
		return fail(stderr, err)
	}
	for _, cl := range out.Clusters {
		for _, a := range cl.Accelerators {
			if err := printPrefixed(stdout, a, "cluster", cl.Name); err != nil {
				return fail(stderr, err)
			}
		}
	}
	if len(out.Clusters) == 0 {
		fmt.Fprintln(stderr, "hl: capacity: no cluster reports capacity")
	}
	return 0
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
		pe := fs.String("platform-experiment", "", "platform experiment (default $PLATFORM_EXPERIMENT_ID)")
		limit := fs.Int("limit", 0, "page size (server default 20, max 200)")
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		q := url.Values{}
		if p := orEnv(*pe, platformExperimentEnv); p != "" {
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
		agent := fs.String("agent", "", "requesting agent (default $AGENT_ID)")
		pe := fs.String("platform-experiment", "", "platform experiment (default $PLATFORM_EXPERIMENT_ID)")
		if err := fs.Parse(rest); err != nil {
			return parseExit(err)
		}
		*agent, *pe = orEnv(*agent, agentEnv), orEnv(*pe, platformExperimentEnv)
		if *credits <= 0 || *reason == "" || *agent == "" || *pe == "" {
			return parseExit(misuse(fs, "--credits above 0, --reason, --agent and --platform-experiment are required"))
		}
		method, path = "POST", "/donations"
		body = map[string]any{"agent_id": *agent, "platform_experiment_id": *pe, "credits_want": *credits, "reason": *reason}
	case "fulfill":
		fs := newFlags("donation fulfill", "usage: hl donation fulfill ID [--agent A]\n\nTransfers the request's credits from your quota to the requester's.", stderr)
		agent := fs.String("agent", "", "donating agent (default $AGENT_ID)")
		id, err := parseOne(fs, rest, "ID", "", "id")
		if err != nil {
			return parseExit(err)
		}
		if *agent = orEnv(*agent, agentEnv); *agent == "" {
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
