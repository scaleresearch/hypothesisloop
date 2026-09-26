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

const submitUsage = `usage: hl hypothesis submit --text T --builds-on ID|root [--builds-on ID ...]
         [--contradicts ID ...] [--related ID ...] [--distinct-from ID ...]
   or: hl hypothesis submit --file hypothesis.yaml [flags above override every item]

Every hypothesis builds on the objective (root) or on existing hypotheses; several --builds-on
mean a combination. While your text reads close to hypotheses it does not build on, or a
--builds-on target is refuted, nothing is registered and the reply lists them: test the one you meant under its id, or
resubmit naming each with --contradicts or --distinct-from.

Who registered it is whoever HL_TOKEN is, and nothing you write names them. --agent (default
$AGENT_ID) says whose hypothesis it is: an agent's token registers its own, and a person or an
API key names an agent to register for it, or names none for a hypothesis no agent owns.

FILE shape:

  agent_id: agent-123             # optional, as --agent
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

var hypothesisCommands = []subcommand{
	{"submit", "register an idea under the objective or the hypotheses it builds on", hypothesisSubmit},
	{"link", "add a link from your own hypothesis", hypothesisLink},
	{"comment", "add a note to a hypothesis", hypothesisComment},
	{"status", "set your hypothesis's verdict", hypothesisStatus},
	{"list", "hypotheses, most recent first", hypothesisList},
	{"get", "one hypothesis with its links, jobs, findings and comments", hypothesisGet},
	{"query", "read-only Cypher over the graph, including its text search", hypothesisQuery},
}

func cmdHypothesis(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "comments" || args[0] == "links") {
		fmt.Fprintf(stderr, "hl: hypothesis %s: read them with hl hypothesis get ID, add one with hl hypothesis %s\n", args[0], strings.TrimSuffix(args[0], "s"))
		return 2
	}
	return runGroup("hypothesis", hypothesisCommands, c, args, stdout, stderr)
}

func hypothesisSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis submit", submitUsage, stderr)
	agent := fs.String("agent", "", "agent whose hypothesis it is (default $AGENT_ID, else the caller's own)")
	pe := fs.String("platform-experiment", "", "platform experiment id (default $PLATFORM_EXPERIMENT_ID)")
	text := fs.String("text", "", "hypothesis text")
	file := fs.String("file", "", "YAML file of one hypothesis, or a hypotheses: list of them (see --help)")
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

	owner, err := recordOwner(c, *agent)
	if err != nil {
		return fail(stderr, err)
	}
	bodies, keys, err := buildSubmitBodies(items, flagLinks, distinctFrom, map[string]string{
		"agent_id": owner, "platform_experiment_id": orEnv(*pe, platformExperimentEnv), "text": *text,
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
			item := ""
			if len(bodies) > 1 {
				item = fmt.Sprintf("item %d: ", i)
			}
			if reportHold(stderr, item, err) {
				return 1
			}
			return fail(stderr, fmt.Errorf("%s%w", item, err))
		}
		var created struct {
			ID      string `json:"id"`
			AgentID string `json:"agent_id"`
			Actor   struct {
				Name string `json:"name"`
			} `json:"actor"`
			AlreadyExisted bool `json:"already_existed"`
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
			by := created.AgentID
			if by == "" {
				by = created.Actor.Name
			}
			fmt.Fprintf(stderr, "hl: hypothesis submit: already registered as %s by %s; your links were not applied, its own are in \"links\". "+
				"Test it as it is, or register a refinement with --builds-on %s\n", created.ID, by, created.ID)
		}
	}
	return 0
}

// reportHold answers a submit held back for the hypotheses in the reply's similar list with those
// hypotheses, one per line, and the two ways on. It reports whether err was such a hold.
func reportHold(stderr io.Writer, item string, err error) bool {
	var refused *apiclient.HTTPError
	var reply struct {
		Similar []json.RawMessage `json:"similar"`
	}
	if !errors.As(err, &refused) || json.Unmarshal([]byte(refused.Body), &reply) != nil || len(reply.Similar) == 0 {
		return false
	}
	fmt.Fprintf(stderr, "hl: hypothesis submit: %snothing registered, yours reads close to these or builds on a refuted one:\n", item)
	ids := make([]string, len(reply.Similar))
	for i, raw := range reply.Similar {
		var h struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &h)
		ids[i] = h.ID
		_ = printLine(stderr, raw)
	}
	fmt.Fprintf(stderr, "hl: hypothesis submit: judge each. The same claim as yours: register nothing and test it with "+
		"hl job submit FILE --hypothesis ID; its verdict stays its owner's. A different claim, narrower and broader ones "+
		"included: rerun adding --distinct-from %s, with --contradicts ID instead for one that cannot hold together with yours.\n",
		strings.Join(ids, ","))
	return true
}

type submitBody struct {
	AgentID              string   `json:"agent_id,omitempty"`
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
		if _, stated := item["author"]; stated {
			return nil, nil, itemErr(items, i, fmt.Errorf("author is not a field: who registers a hypothesis is whoever HL_TOKEN is, and the platform refuses a request that states it. Remove it; agent_id says whose hypothesis it is, and is optional"))
		}
		fields := map[string]string{}
		for _, name := range []string{"agent_id", "platform_experiment_id", "text"} {
			v, err := stringField(item, name)
			if err != nil {
				return nil, nil, itemErr(items, i, err)
			}
			if o := overrides[name]; o != "" {
				v = o
			}
			fields[name] = v
		}
		if fields["platform_experiment_id"] == "" || fields["text"] == "" {
			return nil, nil, itemErr(items, i, fmt.Errorf("platform_experiment_id and text are required (via --platform-experiment/--text or --file)"))
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
	fs := newFlags("hypothesis link", `usage: hl hypothesis link ID --type builds_on|contradicts|related --to ID

Adds one link from your own hypothesis ID (--from ID works too), or from one no agent owns.
Links are append-only. Prints the updated outgoing links. A person or an API key linking from
an agent's hypothesis names that agent with --agent, and is kept as who made the link.`, stderr)
	agent := fs.String("agent", "", "agent whose hypothesis ID is (default $AGENT_ID, else the caller's own)")
	typ := fs.String("type", "", "builds_on, contradicts or related")
	to := fs.String("to", "", "hypothesis id the link points at")
	from, err := parseOne(fs, args, "ID", "", "id", "from")
	if err != nil {
		return parseExit(err)
	}
	if *typ == "" || *to == "" {
		return parseExit(misuse(fs, "--type and --to are required"))
	}
	if !validLinkType(*typ) {
		return parseExit(misuse(fs, "--type must be one of %s, got %q", strings.Join(linkTypes, ", "), *typ))
	}
	owner, err := recordOwner(c, *agent)
	if err != nil {
		return fail(stderr, err)
	}
	body := map[string]string{"type": *typ, "to": *to}
	if owner != "" {
		body["agent_id"] = owner
	}
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
	fs := newFlags("hypothesis comment", `usage: hl hypothesis comment ID --text T

A note on any hypothesis with no job behind it: a verified fact, a dead end, a revision. It is
written by whoever HL_TOKEN is. An agent's token writes its own agent's note; a person or an
API key writes their own, or an agent's with --agent.`, stderr)
	agent := fs.String("agent", "", "agent whose note it is (default $AGENT_ID, else the caller's own)")
	text := fs.String("text", "", "the note")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	if *text == "" {
		return parseExit(misuse(fs, "--text is required"))
	}
	owner, err := recordOwner(c, *agent)
	if err != nil {
		return fail(stderr, err)
	}
	body := map[string]string{"text": *text}
	if owner != "" {
		body["agent_id"] = owner
	}
	var out json.RawMessage
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(id)+"/comments", body, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis status", `usage: hl hypothesis status ID open|inconclusive | confirmed|refuted --against CONTROL_JOB | confirmed|refuted --despite REASON

Sets the verdict on your own hypothesis, or on one no agent owns. --status VERDICT works too.
A person or an API key ruling on an agent's hypothesis names that agent with --agent, and is
kept as who gave the verdict.
confirmed and refuted say the hypothesis's jobs differ from a control, so they need the record
to show it: --against names the control, a completed job of this experiment (a baseline run, or
the same job without your change), and the hypothesis has two completed jobs of its own besides
it. One run with no measured control is inconclusive. When the record cannot show what you know
(a failure that needs no repeat, a control measured outside this experiment), --despite takes a
sentence of why the verdict holds anyway. What a verdict rests on is kept as a comment.`, stderr)
	agent := fs.String("agent", "", "agent whose hypothesis ID is (default $AGENT_ID, else the caller's own)")
	status := fs.String("status", "", "")
	against := fs.String("against", "", "the control job a confirmed or refuted verdict compares with")
	despite := fs.String("despite", "", "why a confirmed or refuted verdict holds without a repeated run and a control")
	operands, err := parseOperands(fs, args, "id")
	if err != nil {
		return parseExit(err)
	}
	if len(operands) == 2 && *status == "" {
		*status, operands = operands[1], operands[:1]
	}
	if len(operands) != 1 || *status == "" {
		return parseExit(misuse(fs, "want ID and a verdict"))
	}
	id := operands[0]
	var out json.RawMessage
	owner, err := recordOwner(c, *agent)
	if err != nil {
		return fail(stderr, err)
	}
	body := map[string]string{"status": *status}
	if owner != "" {
		body["agent_id"] = owner
	}
	if *against != "" {
		body["against"] = *against
	}
	if *despite != "" {
		body["despite"] = *despite
	}
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(id)+"/status", body, &out); err != nil {
		return fail(stderr, err)
	}
	if err := printLine(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hypothesisList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis list", `usage: hl hypothesis list [--agent A] [--status S] [--limit N] [--offset N]

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
	var out []json.RawMessage
	header, err := c.DoHeader("GET", "/hypotheses?"+q.Encode(), nil, &out)
	if err != nil {
		return fail(stderr, err)
	}
	for _, h := range out {
		if err := printLine(stdout, h); err != nil {
			return fail(stderr, err)
		}
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "hl: hypothesis list: no hypothesis matches")
	}
	if total, _ := strconv.Atoi(header.Get("X-Total-Count")); total > *offset+len(out) {
		fmt.Fprintf(stderr, "hl: hypothesis list: %d more; --limit N (max 200) and --offset N show them\n", total-*offset-len(out))
	}
	return 0
}

func hypothesisGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis get", `usage: hl hypothesis get ID [--limit N]

One hypothesis with its links (in and out), findings, comments and jobs, oldest first. A job is
as brief as in hl job list, metrics included, with the env that differs between its jobs (all
of it for an only job); hl job status ID prints its whole record.`, stderr)
	limit := fs.Int("limit", 0, "how many of its jobs, findings and comments each (server default 20, max 200)")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	path := "/hypotheses/" + url.PathEscape(id)
	if *limit != 0 {
		path += "?limit=" + strconv.Itoa(*limit)
	}
	var out json.RawMessage
	if err := c.Do("GET", path, nil, &out); err != nil {
		return fail(stderr, err)
	}
	var h struct {
		Jobs     []json.RawMessage `json:"jobs"`
		JobCount int               `json:"job_count"`
	}
	if err := json.Unmarshal(out, &h); err != nil {
		return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
	}
	jobs, err := briefJobs(h.Jobs)
	if err != nil {
		return fail(stderr, err)
	}
	line, err := compactJSON(out, "jobs")
	if err != nil {
		return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
	}
	fmt.Fprintf(stdout, "%s,\"jobs\":[%s]}\n", line[:len(line)-1], bytes.Join(jobs, []byte(",")))
	if h.JobCount > len(jobs) {
		fmt.Fprintf(stderr, "hl: hypothesis get: %d more jobs; --limit N (max 200) shows them\n", h.JobCount-len(jobs))
	}
	return 0
}

func hypothesisQuery(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis query", `usage: hl hypothesis query 'CYPHER' [--param k=v ...]
   or: hl hypothesis query - [--param k=v ...]   (query read from stdin)

Read-only Cypher over one platform experiment's hypothesis graph (default
$PLATFORM_EXPERIMENT_ID), bound as $pe; writes are discarded: use submit/link. Filter
every MATCH and text search with platform_experiment_id = $pe: a result holding another
experiment's nodes is refused. Prints a JSON object per row keyed by column, 200 rows max.

Schema:
  (:Objective {id})   the root, id = $pe
  (:Hypothesis {id, platform_experiment_id, agent_id, source, actor_kind, actor_id, text, status,
                created_at, status_changed_at, status_actor_kind, status_actor_id})
      the times are datetimes: compare with datetime('RFC3339')
      agent_id: whose hypothesis it is, '' on one no agent owns
      actor_kind (user, key, agent, coordinator, staff) and actor_id: the credential that
      registered it; status_actor_*: the one that gave the verdict. A node from before these
      were kept has neither, and author holds the name it stated then
  (:Hypothesis)-[:BUILDS_ON]->(:Hypothesis or :Objective)
  (:Hypothesis)-[:CONTRADICTS]->(:Hypothesis), (:Hypothesis)-[:RELATED]->(:Hypothesis)
  every relationship {agent_id, actor_kind, actor_id, created_at}
$evidence[h.id]: counts {jobs, each lowercase job status, findings, comments} and
last_finding_at, or null. Text: hl hypothesis get ID.

Examples:

  text search on plain words, best matches first:
  hl hypothesis query 'CALL text_search.search_all("hypothesis_text", "learning rate warmup")
    YIELD node, score WHERE node.platform_experiment_id = $pe
    RETURN node.id, node.status, node.text ORDER BY score DESC LIMIT 10'

  the approaches, the children of the root:
  hl hypothesis query 'MATCH (h:Hypothesis)-[:BUILDS_ON]->(:Objective {id: $pe})
    RETURN h.id, h.status, h.text'

  one branch, everything that builds on a hypothesis at any depth:
  hl hypothesis query 'MATCH (d:Hypothesis)-[:BUILDS_ON*]->(:Hypothesis {id: $id})
    RETURN DISTINCT d.id, d.status, d.text' --param id=HYP_ID

  by job count, untested first:
  hl hypothesis query 'MATCH (h:Hypothesis {platform_experiment_id: $pe})
    RETURN h.id, h.text, coalesce($evidence[h.id].jobs, 0) AS n ORDER BY n'`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment whose graph is read, bound as $pe (default $PLATFORM_EXPERIMENT_ID)")
	var params stringList
	fs.Var(&params, "param", "k=v, bound as $k (string value; repeatable)")
	paramsJSON := fs.String("params", "", "")
	cypher, err := parseOne(fs, args, "query", "", "cypher")
	if err != nil {
		return parseExit(err)
	}
	if *paramsJSON != "" {
		fmt.Fprintln(stderr, "hl: hypothesis query: $pe and $evidence are already bound; pass any other parameter as --param k=v")
		return 2
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
	if strings.HasPrefix(strings.TrimSpace(cypher), "{") {
		return parseExit(misuse(fs, "want the Cypher text itself, got a JSON object"))
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
