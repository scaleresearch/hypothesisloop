package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

func cmdSituation(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("situation", `usage: hl situation [--json] [--platform-experiment PE]

The platform experiment as it stands now, one line per block, each field as key=value:
  run         now, status, ends_at, minutes_left and the stage: its index and name,
              next_boundary_at and next_share_acch, the share that boundary brings. A closed run
              reads minutes_left=0 with closed_at and close_reason; one not started reads
              not_started=true and its duration_hours
  objective   one line per declared metric: direction, deterministic=true on one declared so
              (its values compare across machines and need no repeat), baseline_value, best_value with the
              best_job and best_agent that set it, target, the value the metric's
              target_vs_baseline sets against baseline_value, gap_to_target_pct, and best_1h_ago
              and best_3h_ago, the best as it stood then
  capacity    fits_now, how many more jobs shaped like the baseline's job would be placed now, and
              jobs_in_flight_run, the run's jobs queued and running; then one line per cluster
              and resource the run's jobs use, free and total
  budget      run_budget_acch and run_used_acch of the whole run; for an agent its share, used,
              and room_guaranteed and room_burst, the largest estimate a job of that tier is
              admitted with now
  agents      one line per agent: jobs queued and running, subagents live, turn (working or
              waiting, and since when), activity, minutes_without_job_in_flight, messages
              (left_this_hour it may still be sent, next_at when the next frees up) and model
              (window, utilization of the model allowance, refused_until while it refuses)
  ideas_from_people open, how many ideas people registered that no agent has settled or
              built on yet, and list, the command that lists them
  coordinator summary_written_at, jobs_completed_since_summary and model, for the coordinator
An agent's token reads its own line of agents, a coordinator's every line. A field with no value
is left out. Every figure is read when the command runs. --json prints the record as the
platform answers it.`, stderr)
	named := experimentFlag(fs)
	raw := fs.Bool("json", false, "print the platform's JSON record")
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	pe, err := namedExperiment(fs, c, *named)
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/platform-experiments/"+url.PathEscape(pe)+"/situation", nil, &out); err != nil {
		return fail(stderr, err)
	}
	if *raw {
		fmt.Fprintln(stdout, string(bytes.TrimSpace(out)))
		return 0
	}
	text, err := situationLines(out)
	if err != nil {
		return fail(stderr, fmt.Errorf("situation: %w", err))
	}
	fmt.Fprint(stdout, text)
	return 0
}

// situationLines renders each top-level block of record on its own line, its fields flattened
// to key=value in the platform's order. A list of objects is one line per element, under the
// block's name, or under block.key for a list inside a block.
func situationLines(record []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(record))
	dec.UseNumber()
	root, err := decodeOrdered(dec)
	if err != nil {
		return "", fmt.Errorf("response is not JSON: %w", err)
	}
	if root.kind != '{' {
		return "", fmt.Errorf("response is no object: %s", record)
	}
	var b strings.Builder
	for _, field := range root.fields {
		renderBlock(&b, field.key, field.value)
	}
	return b.String(), nil
}

type orderedJSON struct {
	kind     byte // '{', '[', 's' string, 'v' number or bool, 'n' null
	fields   []orderedField
	elements []*orderedJSON
	text     string
}

type orderedField struct {
	key   string
	value *orderedJSON
}

func decodeOrdered(dec *json.Decoder) (*orderedJSON, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		n := &orderedJSON{kind: byte(t)}
		for dec.More() {
			if t == '{' {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				value, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				n.fields = append(n.fields, orderedField{key, value})
				continue
			}
			element, err := decodeOrdered(dec)
			if err != nil {
				return nil, err
			}
			n.elements = append(n.elements, element)
		}
		_, err := dec.Token()
		return n, err
	case string:
		return &orderedJSON{kind: 's', text: t}, nil
	case json.Number:
		return &orderedJSON{kind: 'v', text: t.String()}, nil
	case bool:
		return &orderedJSON{kind: 'v', text: strconv.FormatBool(t)}, nil
	}
	return &orderedJSON{kind: 'n'}, nil
}

func renderBlock(b *strings.Builder, label string, n *orderedJSON) {
	switch n.kind {
	case '{':
		var pairs []string
		var lists []orderedField
		flattenFields("", n, &pairs, &lists)
		if len(pairs) > 0 {
			fmt.Fprintf(b, "%s: %s\n", label, strings.Join(pairs, " "))
		}
		for _, list := range lists {
			for _, element := range list.value.elements {
				renderBlock(b, label+"."+list.key, element)
			}
		}
	case '[':
		if len(n.elements) == 0 {
			fmt.Fprintf(b, "%s: none\n", label)
		} else if scalars(n) {
			fmt.Fprintf(b, "%s: %s\n", label, joinScalars(n))
		}
		for _, element := range n.elements {
			if element.kind == '{' || element.kind == '[' {
				renderBlock(b, label, element)
			}
		}
	case 's', 'v':
		fmt.Fprintf(b, "%s: %s\n", label, scalarText(n))
	}
}

// flattenFields appends n's fields under prefix as key=value to pairs, an object's fields as
// key.field, and a list of objects to lists, rendered as lines of their own.
func flattenFields(prefix string, n *orderedJSON, pairs *[]string, lists *[]orderedField) {
	for _, field := range n.fields {
		key, v := prefix+field.key, field.value
		switch {
		case v.kind == '{':
			flattenFields(key+".", v, pairs, lists)
		case v.kind == '[' && len(v.elements) == 0:
		case v.kind == '[' && scalars(v):
			*pairs = append(*pairs, key+"="+joinScalars(v))
		case v.kind == '[':
			*lists = append(*lists, orderedField{key, v})
		case v.kind == 's' && v.text == "", v.kind == 'n':
		default:
			*pairs = append(*pairs, key+"="+scalarText(v))
		}
	}
}

func scalars(n *orderedJSON) bool {
	for _, element := range n.elements {
		if element.kind == '{' || element.kind == '[' {
			return false
		}
	}
	return true
}

func joinScalars(n *orderedJSON) string {
	texts := make([]string, 0, len(n.elements))
	for _, element := range n.elements {
		texts = append(texts, scalarText(element))
	}
	return strings.Join(texts, ",")
}

// scalarText is a value as a reader takes it in at a glance: a number to six significant
// digits, a time to the second, and a string with a space, a quote or an = quoted.
func scalarText(n *orderedJSON) string {
	switch n.kind {
	case 'n':
		return "null"
	case 'v':
		if !strings.ContainsAny(n.text, ".eE") {
			return n.text
		}
		f, err := strconv.ParseFloat(n.text, 64)
		if err != nil {
			return n.text
		}
		f, _ = strconv.ParseFloat(strconv.FormatFloat(f, 'g', 6, 64), 64)
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	if t, err := time.Parse(time.RFC3339Nano, n.text); err == nil {
		return t.Truncate(time.Second).Format(time.RFC3339)
	}
	if n.text == "" || strings.ContainsAny(n.text, " \t\n\"=") {
		return strconv.Quote(n.text)
	}
	return n.text
}
