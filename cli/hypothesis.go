package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

const rootTarget = "root"

var linkTypes = []string{"builds_on", "contradicts", "related"}

const placementHelp = "place the idea in the graph: --builds-on root for a fundamentally new approach to the objective, " +
	"or --builds-on ID for each hypothesis it refines, extends or combines " +
	"(find candidates with 'hl hypothesis query' or 'hl hypothesis list')"

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// idList takes one id or a comma-separated list: agents write --builds-on a,b as often as they
// repeat the flag, and no id contains a comma.
type idList []string

func (s *idList) String() string { return strings.Join(*s, ",") }
func (s *idList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

type link struct {
	Type string `json:"type"`
	To   string `json:"to"`
}

const submitUsage = `usage: hl hypothesis submit [--agent A] [--platform-experiment PE] --text T --builds-on ID|root
         [--builds-on ID ...] [--contradicts ID ...] [--related ID ...] [--distinct-from ID ...]
   or: hl hypothesis submit --file hypothesis.yaml [flags above override every item]

Every hypothesis builds on the objective (root) or on existing hypotheses; several --builds-on
mean a combination. While your text reads close to existing hypotheses, or a --builds-on target is
refuted, nothing is registered and the reply lists them: test the one you meant under its id, or
resubmit naming each with --contradicts or --distinct-from. FILE shape:

  agent_id: agent-123
  platform_experiment_id: pe-123
  hypotheses:
    - key: warmup                 # local name, only for links later in this file
      text: "idea one"
      links: [{type: builds_on, to: root}]
    - text: "idea two, refining idea one"
      links: [{type: builds_on, to: warmup}, {type: related, to: HYP_ID}]
      distinct_from: [HYP_ID]     # hypotheses a refused submit listed, judged different

Items are submitted in order; top-level fields default onto every item. Prints one JSON object
per item. Full reference: controlplane/settings/examples/hypothesis.yaml`

const hypothesisUsage = `usage: hl hypothesis <command> ...

  submit   register an idea under the objective or the hypotheses it builds on
  link     add a builds_on, contradicts or related link from your own hypothesis
  comment  add a note to a hypothesis
  status   set your hypothesis's verdict
  list     one platform experiment's hypotheses, most recent first
  get      one hypothesis with its links, jobs, findings and comments
  query    read-only Cypher over the graph, including its text search

Run hl hypothesis <command> --help for its flags.`

func cmdHypothesis(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, hypothesisUsage)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, hypothesisUsage)
		return 0
	case "submit":
		return hypothesisSubmit(c, rest, stdout, stderr)
	case "link":
		return hypothesisLink(c, rest, stdout, stderr)
	case "comment":
		return hypothesisComment(c, rest, stdout, stderr)
	case "status":
		return hypothesisStatus(c, rest, stdout, stderr)
	case "list":
		return hypothesisList(c, rest, stdout, stderr)
	case "get":
		return hypothesisGet(c, rest, stdout, stderr)
	case "query":
		return hypothesisQuery(c, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hl: hypothesis: unknown subcommand %q\n\n%s\n", sub, hypothesisUsage)
		return 2
	}
}

func hypothesisSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis submit", submitUsage, stderr)
	agent := fs.String("agent", "", "agent id (default $AGENT_ID)")
	pe := fs.String("platform-experiment", "", "platform experiment id (default $PLATFORM_EXPERIMENT_ID)")
	text := fs.String("text", "", "hypothesis text")
	file := fs.String("file", "", "YAML file shaped like the POST /hypotheses body, or a hypotheses: list of them")
	var buildsOn, contradicts, related, distinctFrom idList
	fs.Var(&buildsOn, "builds-on", "hypothesis id this refines, extends or combines, or root for the objective (repeatable or comma-separated, at least one)")
	fs.Var(&contradicts, "contradicts", "hypothesis id that cannot hold together with this one (repeatable or comma-separated)")
	fs.Var(&related, "related", "hypothesis id to cross-reference (repeatable or comma-separated)")
	fs.Var(&distinctFrom, "distinct-from", "hypothesis id a refused submit listed that you judged a different claim (repeatable or comma-separated)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}

	items := []map[string]any{{}}
	if *file != "" {
		raw, err := readYAMLItems(*file, "hypotheses")
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
		items = raw
	}
	var flagLinks []link
	for _, group := range []struct {
		typ string
		ids idList
	}{{"builds_on", buildsOn}, {"contradicts", contradicts}, {"related", related}} {
		for _, id := range group.ids {
			flagLinks = append(flagLinks, link{Type: group.typ, To: id})
		}
	}

	bodies, keys, err := buildSubmitBodies(items, flagLinks, distinctFrom, map[string]string{
		"agent_id": orEnv(*agent, agentEnv), "platform_experiment_id": orEnv(*pe, platformExperimentEnv), "text": *text,
	})
	if err != nil {
		return parseExit(misuse(fs, "%s", err))
	}

	ids := map[string]string{}
	for i, body := range bodies {
		for j, l := range body.Links {
			if id, ok := ids[l.To]; ok {
				body.Links[j].To = id
			}
		}
		for j, d := range body.DistinctFrom {
			if id, ok := ids[d]; ok {
				body.DistinctFrom[j] = id
			}
		}
		var out json.RawMessage
		if err := c.Do("POST", "/hypotheses", body, &out); err != nil {
			if len(bodies) > 1 {
				return fail(stderr, fmt.Errorf("item %d: %w", i, err))
			}
			return fail(stderr, err)
		}
		var created struct {
			ID             string `json:"id"`
			AgentID        string `json:"agent_id"`
			Author         string `json:"author"`
			AlreadyExisted bool   `json:"already_existed"`
		}
		if err := json.Unmarshal(out, &created); err != nil || created.ID == "" {
			return fail(stderr, fmt.Errorf("item %d: response carries no id: %s", i, out))
		}
		if keys[i] != "" {
			ids[keys[i]] = created.ID
		}
		if err := printLine(stdout, out); err != nil {
			return fail(stderr, err)
		}
		if created.AlreadyExisted {
			fmt.Fprintf(stderr, "hl: hypothesis submit: already registered as %s by %s; your links were not applied, its own are in \"links\". "+
				"Test it as it is, or register a refinement with --builds-on %s\n", created.ID, created.AgentID+created.Author, created.ID)
		}
	}
	return 0
}

type submitBody struct {
	AgentID              string   `json:"agent_id,omitempty"`
	Author               string   `json:"author,omitempty"`
	PlatformExperimentID string   `json:"platform_experiment_id"`
	Text                 string   `json:"text"`
	Links                []link   `json:"links"`
	DistinctFrom         []string `json:"distinct_from,omitempty"`
}

// buildSubmitBodies validates every item before anything is sent, so a bad item late in a file
// never leaves the earlier ones half submitted. Links to a local key stay as the key here and are
// swapped for the id the earlier item came back with at submit time.
func buildSubmitBodies(items []map[string]any, flagLinks []link, flagDistinct []string, overrides map[string]string) ([]*submitBody, []string, error) {
	keyIndex := map[string]int{}
	keys := make([]string, len(items))
	for i, item := range items {
		key, err := stringField(item, "key")
		if err != nil {
			return nil, nil, itemErr(items, i, err)
		}
		if key == "" {
			continue
		}
		if key == rootTarget {
			return nil, nil, itemErr(items, i, fmt.Errorf("key %q is reserved for the objective", rootTarget))
		}
		if prev, dup := keyIndex[key]; dup {
			return nil, nil, itemErr(items, i, fmt.Errorf("key %q already used by item %d", key, prev))
		}
		keyIndex[key] = i
		keys[i] = key
	}

	bodies := make([]*submitBody, len(items))
	for i, item := range items {
		fields := map[string]string{}
		for _, name := range []string{"agent_id", "author", "platform_experiment_id", "text"} {
			v, err := stringField(item, name)
			if err != nil {
				return nil, nil, itemErr(items, i, err)
			}
			if o := overrides[name]; o != "" {
				v = o
			}
			fields[name] = v
		}
		if fields["platform_experiment_id"] == "" || fields["text"] == "" || (fields["agent_id"] == "" && fields["author"] == "") {
			return nil, nil, itemErr(items, i, fmt.Errorf("agent_id (or author), platform_experiment_id and text are required (via --agent/--platform-experiment/--text or --file)"))
		}
		links := flagLinks
		if len(links) == 0 {
			parsed, err := parseLinks(item["links"])
			if err != nil {
				return nil, nil, itemErr(items, i, err)
			}
			links = parsed
		}
		resolved := make([]link, 0, len(links))
		placed := false
		for _, l := range links {
			if !validLinkType(l.Type) {
				return nil, nil, itemErr(items, i, fmt.Errorf("link type %q must be one of %s", l.Type, strings.Join(linkTypes, ", ")))
			}
			if l.To == "" {
				return nil, nil, itemErr(items, i, fmt.Errorf("%s link has no target", l.Type))
			}
			if l.To == rootTarget {
				if l.Type != "builds_on" {
					return nil, nil, itemErr(items, i, fmt.Errorf("only builds_on may target root"))
				}
				l.To = fields["platform_experiment_id"]
			} else if at, ok := keyIndex[l.To]; ok && at >= i {
				return nil, nil, itemErr(items, i, fmt.Errorf("link to key %q, which item %d defines; list a parent before its children", l.To, at))
			}
			placed = placed || l.Type == "builds_on"
			resolved = append(resolved, l)
		}
		if !placed {
			return nil, nil, itemErr(items, i, fmt.Errorf("at least one --builds-on (or a builds_on entry in links:) is required; %s", placementHelp))
		}
		distinct := flagDistinct
		if len(distinct) == 0 {
			parsed, err := parseIDs(item["distinct_from"])
			if err != nil {
				return nil, nil, itemErr(items, i, err)
			}
			distinct = parsed
		}
		bodies[i] = &submitBody{
			AgentID:              fields["agent_id"],
			Author:               fields["author"],
			PlatformExperimentID: fields["platform_experiment_id"],
			Text:                 fields["text"],
			Links:                resolved,
			DistinctFrom:         slices.Clone(distinct),
		}
	}
	return bodies, keys, nil
}

func itemErr(items []map[string]any, i int, err error) error {
	if len(items) > 1 {
		return fmt.Errorf("item %d: %w", i, err)
	}
	return err
}

func stringField(item map[string]any, name string) (string, error) {
	v, ok := item[name]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %v", name, v)
	}
	return s, nil
}

func parseLinks(v any) ([]link, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("links must be a list of {type, to}")
	}
	out := make([]link, 0, len(list))
	for j, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("links[%d] must be a mapping {type, to}", j)
		}
		for k := range m {
			if k != "type" && k != "to" {
				return nil, fmt.Errorf("links[%d]: unknown field %q (want type, to)", j, k)
			}
		}
		typ, err := stringField(m, "type")
		if err != nil {
			return nil, fmt.Errorf("links[%d]: %w", j, err)
		}
		to, err := stringField(m, "to")
		if err != nil {
			return nil, fmt.Errorf("links[%d]: %w", j, err)
		}
		out = append(out, link{Type: typ, To: to})
	}
	return out, nil
}

func parseIDs(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("distinct_from must be a list of hypothesis ids")
	}
	out := make([]string, 0, len(list))
	for j, e := range list {
		id, ok := e.(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("distinct_from[%d] must be a hypothesis id", j)
		}
		out = append(out, id)
	}
	return out, nil
}

func validLinkType(t string) bool {
	for _, lt := range linkTypes {
		if t == lt {
			return true
		}
	}
	return false
}

func hypothesisLink(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis link", `usage: hl hypothesis link ID --type builds_on|contradicts|related --to ID [--agent A]

Adds one link from your own hypothesis ID (--from ID works too). Links are append-only. Prints
the updated outgoing links.`, stderr)
	agent := fs.String("agent", "", "agent id, the one that registered ID (default $AGENT_ID)")
	typ := fs.String("type", "", "builds_on, contradicts or related")
	to := fs.String("to", "", "hypothesis id the link points at")
	from, err := parseOne(fs, args, "ID", "", "id", "from")
	if err != nil {
		return parseExit(err)
	}
	*agent = orEnv(*agent, agentEnv)
	if *agent == "" || *typ == "" || *to == "" {
		return parseExit(misuse(fs, "--agent, --type and --to are required"))
	}
	if !validLinkType(*typ) {
		return parseExit(misuse(fs, "--type must be one of %s, got %q", strings.Join(linkTypes, ", "), *typ))
	}
	body := map[string]string{"agent_id": *agent, "type": *typ, "to": *to}
	var out json.RawMessage
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(from)+"/links", body, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisComment(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis comment", `usage: hl hypothesis comment ID --text T [--agent A]

A note on any hypothesis with no job behind it: a verified fact, a dead end, a revision.`, stderr)
	agent := fs.String("agent", "", "agent id (default $AGENT_ID)")
	text := fs.String("text", "", "the note")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	*agent = orEnv(*agent, agentEnv)
	if *agent == "" || *text == "" {
		return parseExit(misuse(fs, "--agent and --text are required"))
	}
	var out json.RawMessage
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(id)+"/comments", map[string]string{"agent_id": *agent, "text": *text}, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis status", `usage: hl hypothesis status ID --status open|confirmed|refuted|inconclusive [--agent A]

Sets the verdict on your own hypothesis, or on a human-submitted one.`, stderr)
	agent := fs.String("agent", "", "agent id, the one that registered ID (default $AGENT_ID)")
	status := fs.String("status", "", "open, confirmed, refuted or inconclusive")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	*agent = orEnv(*agent, agentEnv)
	if *agent == "" || *status == "" {
		return parseExit(misuse(fs, "--agent and --status are required"))
	}
	var out json.RawMessage
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(id)+"/status", map[string]string{"agent_id": *agent, "status": *status}, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis list", `usage: hl hypothesis list [--platform-experiment PE] [--agent A] [--status S] [--limit N] [--offset N]

Most recent first, one JSON object per line with builds_on, child_count, finding_count and
comment_count. For structure or text search use 'hl hypothesis query'.`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (default $PLATFORM_EXPERIMENT_ID)")
	agent := fs.String("agent", "", "only this agent's hypotheses")
	status := fs.String("status", "", "open, confirmed, refuted or inconclusive")
	limit := fs.Int("limit", 0, "page size (server default 20, max 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	*pe = orEnv(*pe, platformExperimentEnv)
	if *pe == "" {
		return parseExit(misuse(fs, "--platform-experiment is required"))
	}
	q := url.Values{"platform_experiment_id": {*pe}}
	if *agent != "" {
		q.Set("agent", *agent)
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
	var out json.RawMessage
	if err := c.Do("GET", "/hypotheses?"+q.Encode(), nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLines(stdout, stderr, "hypothesis list: no hypothesis matches", out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis get", "usage: hl hypothesis get ID\n\nOne hypothesis with its links (in and out), jobs, findings and comments.", stderr)
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	var out json.RawMessage
	if err := c.Do("GET", "/hypotheses/"+url.PathEscape(id), nil, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisQuery(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis query", `usage: hl hypothesis query 'CYPHER' [--param k=v ...] [--platform-experiment PE]
   or: hl hypothesis query - [--param k=v ...]   (query read from stdin)

Read-only Cypher over one platform experiment's hypothesis graph (default
$PLATFORM_EXPERIMENT_ID), bound as $pe; writes are discarded, use submit/link instead. Filter
every MATCH and text search with platform_experiment_id = $pe: a result holding another
experiment's nodes is refused. $tested holds the ids of its hypotheses that have a job. Prints
one JSON object per row keyed by column. The schema is in the POST /hypotheses/query
description (/explore). Examples:

  text search, best matches first:
  hl hypothesis query 'CALL text_search.search_all("hypothesis_text", "learning rate warmup")
    YIELD node, score WHERE node.platform_experiment_id = $pe
    RETURN node.id, node.status, node.text ORDER BY score DESC LIMIT 10'

  the approaches, the children of the root:
  hl hypothesis query 'MATCH (h:Hypothesis)-[:BUILDS_ON]->(:Objective {id: $pe})
    RETURN h.id, h.status, h.text'

  one branch, everything that builds on a hypothesis at any depth:
  hl hypothesis query 'MATCH (d:Hypothesis)-[:BUILDS_ON*]->(:Hypothesis {id: $id})
    RETURN DISTINCT d.id, d.status, d.text' --param id=HYP_ID

  hypotheses no job has tested yet:
  hl hypothesis query 'MATCH (h:Hypothesis {platform_experiment_id: $pe})
    WHERE NOT h.id IN $tested RETURN h.id, h.status, h.text'`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment whose graph is read, bound as $pe (default $PLATFORM_EXPERIMENT_ID)")
	var params stringList
	fs.Var(&params, "param", "k=v, bound as $k (string value; repeatable)")
	cypher, err := parseOne(fs, args, "query", "", "cypher")
	if err != nil {
		return parseExit(err)
	}
	if cypher == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail(stderr, fmt.Errorf("reading stdin: %w", err))
		}
		cypher = string(raw)
	}
	if strings.TrimSpace(cypher) == "" {
		return parseExit(misuse(fs, "empty query"))
	}
	*pe = orEnv(*pe, platformExperimentEnv)
	if *pe == "" {
		return parseExit(misuse(fs, "--platform-experiment is required (or set PLATFORM_EXPERIMENT_ID)"))
	}
	bound := map[string]any{}
	for _, p := range params {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return parseExit(misuse(fs, "--param wants k=v, got %q", p))
		}
		bound[k] = v
	}
	var out struct {
		Columns   []string            `json:"columns"`
		Rows      [][]json.RawMessage `json:"rows"`
		Truncated bool                `json:"truncated"`
	}
	if err := c.Do("POST", "/hypotheses/query", map[string]any{"platform_experiment_id": *pe, "cypher": cypher, "params": bound}, &out); err != nil {
		return fail(stderr, err)
	}
	for i, row := range out.Rows {
		if len(row) != len(out.Columns) {
			return fail(stderr, fmt.Errorf("row %d has %d values for %d columns", i, len(row), len(out.Columns)))
		}
		var b bytes.Buffer
		if err := json.Compact(&b, rowObject(out.Columns, row)); err != nil {
			return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
		}
		fmt.Fprintf(stdout, "%s\n", b.Bytes())
	}
	if len(out.Rows) == 0 {
		fmt.Fprintln(stderr, "hl: hypothesis query: no rows")
	}
	if out.Truncated {
		fmt.Fprintf(stderr, "hl: hypothesis query: truncated at %d rows; narrow the query or add LIMIT/SKIP\n", len(out.Rows))
	}
	return 0
}

// rowObject keeps the query's column order, which a Go map would lose.
func rowObject(columns []string, row []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, col := range columns {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(col)
		b.Write(name)
		b.WriteByte(':')
		b.Write(row[i])
	}
	b.WriteByte('}')
	return b.Bytes()
}
