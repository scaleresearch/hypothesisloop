package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
   or: hl hypothesis submit --text-file FILE|- --builds-on ID|root [flags above]
   or: hl hypothesis submit --file hypothesis.yaml [flags above override every item]

Every hypothesis builds on the objective (root) or on existing hypotheses; several --builds-on
mean a combination. While your text reads close to hypotheses it does not build on, or a
--builds-on target is refuted, nothing is registered and the reply lists them: test the one you meant under its id, or
resubmit naming each with --contradicts or --distinct-from.

Who registered it is whoever HL_TOKEN is, and nothing you write names them. --agent says whose
hypothesis it is: an agent's token registers its own, and a person or an
API key names an agent to register for it, or names none for a hypothesis no agent owns.

FILE shape:

  agent_id: agent-123             # as --agent; an agent's token says both, leave them out
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
	{"submit", "register an idea under the objective or the hypotheses it builds on", hypothesisSubmit, everyone},
	{"checkout", "put the clone on your branch for a hypothesis, made from what the hypothesis builds on, and record you as working on it", hypothesisCheckout, asAgent},
	{"link", "add a link from your own hypothesis", hypothesisLink, everyone},
	{"comment", "add a note to a hypothesis, or to the objective as root", hypothesisComment, everyone},
	{"status", "set the verdict of your hypothesis or of a person's idea you tested; confirmed and refuted name the control (--against) and need two completed jobs of its own besides it, one where every ranking metric is deterministic, or --despite with why it holds without", hypothesisStatus, everyone},
	{"list", "hypotheses, open ideas of people first, then most recent", hypothesisList, everyone},
	{"get", "one hypothesis with its links, findings, implementations, comments and jobs, short unless --full; root: the comments on the objective", hypothesisGet, everyone},
	{"query", "read-only Cypher over the graph, including its text search", hypothesisQuery, everyone},
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
	agent := fs.String("agent", "", "agent whose hypothesis it is (default the token's agent, else the caller's own)")
	pe := fs.String("platform-experiment", "", "platform experiment id (default the token's)")
	hypothesisText := newTextFlag(fs, "text", "hypothesis text")
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
	text, err := hypothesisText.value(fs)
	if err != nil {
		return parseExit(err)
	}

	items := []map[string]any{{}}
	if *file == "-" {
		if err := fs.claimStdin("--file -"); err != nil {
			return parseExit(err)
		}
	}
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
		"agent_id": owner, "platform_experiment_id": experimentOf(c, *pe), "text": text,
	})
	if err != nil {
		return parseExit(misuse(fs, "%s", err))
	}

	ids := map[string]string{}
	for i, body := range bodies {
		named := func(ref string) (string, error) {
			if id, ok := ids[ref]; ok {
				return id, nil
			}
			if slices.Contains(keys, ref) {
				return ref, nil
			}
			return fullHypothesisID(c, body.PlatformExperimentID, ref)
		}
		for j := range body.Links {
			if body.Links[j].To, err = named(body.Links[j].To); err != nil {
				return fail(stderr, err)
			}
		}
		for j := range body.DistinctFrom {
			if body.DistinctFrom[j], err = named(body.DistinctFrom[j]); err != nil {
				return fail(stderr, err)
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
	if reason, _ := refusalOf(err); reason != "similar_hypotheses" || !errors.As(err, &refused) || json.Unmarshal([]byte(refused.Body), &reply) != nil {
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
		"included: rerun adding --distinct-from %s, with --contradicts ID instead for one that cannot hold together with yours. "+
		"A refuted parent you build on stays your parent: keep its --builds-on and add it to --distinct-from when your claim "+
		"changes what refuted it.\n",
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
			return nil, nil, itemErr(items, i, fmt.Errorf("platform_experiment_id and text are required (via --platform-experiment/--text or --file; an agent's token says its platform experiment)"))
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
	agent := fs.String("agent", "", "agent whose hypothesis ID is (default the token's agent, else the caller's own)")
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
	for _, id := range []*string{&from, to} {
		if *id, err = fullHypothesisID(c, experimentOf(c, ""), *id); err != nil {
			return fail(stderr, err)
		}
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
	fs := newFlags("hypothesis comment", `usage: hl hypothesis comment ID|root --text T | --text-file FILE|-

A note on any hypothesis with no job behind it: a verified fact, a dead end, a revision. root,
or the platform experiment's id, is the objective: a note for every approach, such as the
published results on the problem, which every agent of the platform experiment is told of.
It is written by whoever HL_TOKEN is. An agent's token writes its own agent's note; a person or
an API key writes their own, or an agent's with --agent.`, stderr)
	agent := fs.String("agent", "", "agent whose note it is (default the token's agent, else the caller's own)")
	pe := fs.String("platform-experiment", "", "the platform experiment whose objective root names (default the token's)")
	note := newTextFlag(fs, "text", "the note")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	text, err := note.value(fs)
	if err != nil {
		return parseExit(err)
	}
	if text == "" {
		return parseExit(misuse(fs, "--text or --text-file is required"))
	}
	id, objective, err := objectiveOr(fs, c, id, *pe)
	if err != nil {
		return parseExit(err)
	}
	if !objective {
		if id, err = fullHypothesisID(c, experimentOf(c, *pe), id); err != nil {
			return fail(stderr, err)
		}
	}
	owner, err := recordOwner(c, *agent)
	if err != nil {
		return fail(stderr, err)
	}
	body := map[string]string{"text": text}
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

Sets the verdict on your own hypothesis, or on one no agent owns: a person's idea you tested
under its own id is yours to judge like your own, and stays the person's. --status VERDICT works too.
A person or an API key ruling on an agent's hypothesis names that agent with --agent, and is
kept as who gave the verdict.
Every verdict needs a job of the hypothesis that completed or failed; --despite never replaces it.
confirmed and refuted compare the hypothesis's jobs with a control, so they need the record
to show it: --against names the control, a completed job of this experiment (a baseline run, or
the same job without your change), and the hypothesis has two completed jobs of its own besides
it, or one where every ranking metric is declared deterministic (hl situation). confirmed is
the gain the hypothesis expected, reached: one far below it, or too small to matter against the
spread or the target, is no confirmation. One run with no measured control
is inconclusive. A repeat is needed only where results vary
between runs. When the record cannot show what you know (a deterministic job on a metric not
declared so; a failure that needs no repeat; a control measured outside this experiment),
--despite takes a sentence of why the verdict holds anyway. Where every ranking metric is
declared deterministic, a hypothesis with a completed job of its own needs only the control:
name it with --against (the baseline's job is one), and keep --despite for a verdict no job of
the hypothesis shows. What a verdict rests on is kept as a comment. --despite-file FILE, or -
for stdin, gives REASON in place of --despite REASON.`, stderr)
	agent := fs.String("agent", "", "agent whose hypothesis ID is (default the token's agent, else the caller's own)")
	status := fs.String("status", "", "")
	against := fs.String("against", "", "the control job a confirmed or refuted verdict compares with")
	despiteFlag := newTextFlag(fs, "despite", "why a confirmed or refuted verdict holds without a repeated run and a control")
	operands, err := parseOperands(fs, args, "id")
	if err != nil {
		return parseExit(err)
	}
	despite, err := despiteFlag.value(fs)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) == 2 && *status == "" {
		*status, operands = operands[1], operands[:1]
	}
	if len(operands) != 1 || *status == "" {
		return parseExit(misuse(fs, "want ID and a verdict"))
	}
	id, err := fullHypothesisID(c, experimentOf(c, ""), operands[0])
	if err != nil {
		return fail(stderr, err)
	}
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
	if despite != "" {
		body["despite"] = despite
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
	fs := newFlags("hypothesis list", `usage: hl hypothesis list [--agent A] [--status S] [--by people] [--limit N] [--offset N] [--full]

The open ideas of people first, those no agent has settled or built on yet, then the most recent,
one JSON object per line with builds_on, child_count, finding_count, comment_count and working:
the agents on it now, a job of theirs queued or running or a checkout within two hours. Each
text is cut to its first 240 characters ("…" ends one cut) and status_actor and
platform_experiment_id are left out unless --full; hl hypothesis get ID prints one whole. For
structure or text search use 'hl hypothesis query'. The branches a hypothesis was checked out
or run on are in hl hypothesis get ID, as implementations.`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (default the token's)")
	agent := fs.String("agent", "", "only this agent's hypotheses")
	status := fs.String("status", "", "open, confirmed, refuted or inconclusive")
	by := fs.String("by", "", "people: only the hypotheses a person registered")
	limit := fs.Int("limit", 0, "page size (server default 20, max 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	full := fs.Bool("full", false, "every row whole: texts uncut, status_actor and platform_experiment_id")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return parseExit(misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	if *by != "" && *by != "people" {
		return parseExit(misuse(fs, "--by takes people, not %q", *by))
	}
	*pe = experimentOf(c, *pe)
	if *pe == "" {
		return parseExit(misuse(fs, "--platform-experiment is required"))
	}
	q := url.Values{"platform_experiment_id": {*pe}}
	if *agent != "" {
		q.Set("agent_id", *agent)
	}
	if *status != "" {
		q.Set("status", *status)
	}
	if *by != "" {
		q.Set("by", *by)
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
	cut := 0
	for _, h := range out {
		var drop []string
		if !*full {
			n := 0
			if h, n, err = cutStrings(h, textCut); err != nil {
				return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
			}
			cut += n
			drop = []string{"status_actor", "platform_experiment_id"}
		}
		if err := printLine(stdout, h, drop...); err != nil {
			return fail(stderr, err)
		}
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "hl: hypothesis list: no hypothesis matches")
	}
	if cut > 0 {
		fmt.Fprintf(stderr, "hl: hypothesis list: %d texts cut to %d characters; hl hypothesis get ID or --full prints them whole\n", cut, textCut)
	}
	if total, _ := strconv.Atoi(header.Get("X-Total-Count")); total > *offset+len(out) {
		fmt.Fprintf(stderr, "hl: hypothesis list: %d more; --limit N (max 200) and --offset N show them\n", total-*offset-len(out))
	}
	if *offset == 0 {
		objectiveComments(c, *pe, stderr)
	}
	return 0
}

// commentCut is how many characters of a comment hl hypothesis get prints unless told --full.
const commentCut = 160

// objectiveOr is the id a hypothesis command addresses, and whether it is the objective: root
// names the objective of the platform experiment (flagValue, else the token's), and so does that
// platform experiment's id, which the platform answers for it everywhere.
func objectiveOr(fs *flagSet, c *apiclient.Client, id, flagValue string) (string, bool, error) {
	if id != rootTarget {
		pe := experimentOf(c, flagValue)
		return id, id == pe || strings.HasPrefix(id, "pe-"), nil
	}
	pe, err := namedExperiment(fs, c, flagValue)
	return pe, true, err
}

// objectiveGet prints the objective as hl hypothesis get prints a hypothesis: its comments, cut
// unless full, and how many there are. The rest of the objective is read elsewhere.
func objectiveGet(c *apiclient.Client, pe string, full bool, limit int, stdout, stderr io.Writer) int {
	path := "/hypotheses/" + url.PathEscape(pe) + "/comments"
	if limit != 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var comments []map[string]any
	header, err := c.DoHeader("GET", path, nil, &comments)
	if err != nil {
		return fail(stderr, err)
	}
	total, _ := strconv.Atoi(header.Get("X-Total-Count"))
	if !full {
		for _, comment := range comments {
			for _, key := range []string{"id", "hypothesis_id", "platform_experiment_id", "actor"} {
				delete(comment, key)
			}
			if text, _ := comment["text"].(string); len([]rune(text)) > commentCut {
				comment["text"] = string([]rune(text)[:commentCut]) + "…"
				comment["cut"] = true
			}
		}
	}
	if comments == nil {
		comments = []map[string]any{}
	}
	line, err := json.Marshal(struct {
		ID              string           `json:"id"`
		Objective       bool             `json:"objective"`
		CommentCount    int              `json:"comment_count"`
		CommentsOmitted int              `json:"comments_omitted,omitempty"`
		Comments        []map[string]any `json:"comments"`
	}{pe, true, total, max(total-len(comments), 0), comments})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

// objectiveComments says on stderr how many comments the objective of pe holds, where a list of
// its hypotheses is read: they are on no hypothesis it lists. A failed read says nothing.
func objectiveComments(c *apiclient.Client, pe string, stderr io.Writer) {
	var comments []json.RawMessage
	header, err := c.DoHeader("GET", "/hypotheses/"+url.PathEscape(pe)+"/comments?limit=1", nil, &comments)
	if err != nil {
		return
	}
	if total, _ := strconv.Atoi(header.Get("X-Total-Count")); total > 0 {
		fmt.Fprintf(stderr, "hl: hypothesis list: %d comments on the objective (root): hl hypothesis get root prints them\n", total)
	}
}

// hypothesisBrief is what hl hypothesis get prints of the lists of a hypothesis unless told
// --full: a link without the text of the hypothesis it names, a finding and a comment without
// what the hypothesis already says of them, and the comment cut short.
type hypothesisBrief struct {
	Links struct {
		Out []briefLink `json:"out"`
		In  []briefLink `json:"in"`
	} `json:"links"`
	Findings []struct {
		Job       string `json:"experiment_id"`
		AgentID   string `json:"agent_id"`
		Summary   string `json:"summary"`
		CreatedAt string `json:"created_at"`
	} `json:"findings"`
	Comments []struct {
		AgentID   string `json:"agent_id"`
		Text      string `json:"text"`
		Cut       bool   `json:"cut,omitempty"`
		CreatedAt string `json:"created_at"`
	} `json:"comments"`
}

type briefLink struct {
	Type       string `json:"type"`
	Hypothesis string `json:"hypothesis_id"`
	Status     string `json:"status"`
}

func hypothesisGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis get", `usage: hl hypothesis get ID|root [--full] [--limit N]

root, or the platform experiment's id, prints the objective: the comments on it, oldest first,
cut like a hypothesis's, with comment_count. The objective is the platform experiment's: hl
platform-experiments get has its description and baseline, and its approaches are the
hypotheses that build on root.
One hypothesis on one line, short enough to read whole: its text and status, its links in and
out (type, hypothesis_id and status of each), its findings in full (experiment_id is the job
one is of), its comments cut to their first 160 characters ("cut": true on each one cut), and
its jobs, oldest first, each as id, agent_id, status, metrics and, under job.env, the settings
that differ between its jobs (all of them for an only job). job_count, finding_count and
comment_count say how many there are; comments_omitted and jobs_omitted, how many the page left
out (--limit shows more). implementations lists the branches it was checked out or run on, one per
agent, each with agent_id, branch, code_ref, the commit its newest job there ran, and
checked_out_at: a hypothesis is an idea and any agent may run its own code for it. working is
the agents on it now: a job of theirs queued or running, or a checkout within two hours.
base_ref is the commit a new implementation starts from (hl hypothesis checkout ID).
--full prints the whole record: the text of every linked hypothesis, every comment whole, who
wrote each, and a job as hl job list prints it. hl job status ID prints a job's whole record.`, stderr)
	full := fs.Bool("full", false, "the whole record: comments uncut, linked hypotheses with their text, jobs as in hl job list")
	limit := fs.Int("limit", 0, "how many of its jobs, findings and comments each (server default 20, max 200)")
	pe := fs.String("platform-experiment", "", "the platform experiment whose objective root names (default the token's)")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	id, objective, err := objectiveOr(fs, c, id, *pe)
	if err != nil {
		return parseExit(err)
	}
	if objective {
		return objectiveGet(c, id, *full, *limit, stdout, stderr)
	}
	if id, err = fullHypothesisID(c, experimentOf(c, *pe), id); err != nil {
		return fail(stderr, err)
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
		hypothesisBrief
		Jobs         []json.RawMessage `json:"jobs"`
		JobCount     int               `json:"job_count"`
		CommentCount int               `json:"comment_count"`
	}
	if err := json.Unmarshal(out, &h); err != nil {
		return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
	}
	drop, render := []string{"jobs"}, briefJobs[jobBrief]
	var lists []byte
	if !*full {
		drop = append(drop, "links", "findings", "comments", "actor", "status_actor", "platform_experiment_id")
		render = briefJobs[jobResult]
		for i, comment := range h.Comments {
			if text := []rune(comment.Text); len(text) > commentCut {
				h.Comments[i].Text = string(text[:commentCut]) + "…"
				h.Comments[i].Cut = true
			}
		}
		brief, _ := json.Marshal(h.hypothesisBrief)
		if brief, _ = compactJSON(brief); len(brief) > 2 {
			lists = append(brief[1:len(brief)-1], ',')
		}
	}
	jobs, err := render(h.Jobs)
	if err != nil {
		return fail(stderr, err)
	}
	line, err := compactJSON(out, drop...)
	if err != nil {
		return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
	}
	if n := h.CommentCount - len(h.Comments); n > 0 {
		lists = fmt.Appendf(lists, "\"comments_omitted\":%d,", n)
	}
	if n := h.JobCount - len(jobs); n > 0 {
		lists = fmt.Appendf(lists, "\"jobs_omitted\":%d,", n)
	}
	fmt.Fprintf(stdout, "%s,%s\"jobs\":[%s]}\n", line[:len(line)-1], lists, bytes.Join(jobs, []byte(",")))
	return 0
}

const queryExample = `hl hypothesis query 'MATCH (h:Hypothesis) RETURN h.id, h.status, h.text'`

func hypothesisQuery(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis query", `usage: hl hypothesis query 'CYPHER' [--param k=v ...]
   or: hl hypothesis query - [--param k=v ...]   (query read from stdin)

Read-only Cypher over one platform experiment's hypothesis graph (default
the token's), bound as $pe; writes are discarded: use submit/link. Prints a
JSON object per row keyed by column, 200 rows max, each string value cut to its first
240 characters ("…" ends one cut) unless --full.

A query reads that platform experiment and nothing else: the platform ties every node,
every variable-length relationship and every text search to it before the query runs.
For that it takes this much of Cypher:
  every node names its label, each time it is written: (h:Hypothesis) or (o:Objective).
      (n), () and (h) for a node matched earlier are refused, as is a variable alone in
      parentheses; another label, two labels and a parameter as the property map too
  the scope is set for you: {platform_experiment_id: $pe} and {id: $pe} may be written
      or left out, and any other value for them is refused
  a variable-length relationship takes type, bounds and properties: [:BUILDS_ON*1..3]
  CALL only text_search.search, search_all, regex_search and fuzzy_phrase_search, each
      with YIELD node (and score)
  built-in functions only (count, collect, coalesce, size, nodes, datetime, ...); the
      refusal lists them
  the query starts with MATCH, OPTIONAL MATCH, WITH, UNWIND, RETURN or CALL

Schema:
  (:Objective {id})   the root, id = $pe: there is one, so (:Objective) is it
  (:Hypothesis {id, platform_experiment_id, agent_id, actor_kind, actor_id, text, status,
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
last_finding_at, or null; $evidence[$pe] is the objective's, its jobs the baseline's. Text:
hl hypothesis get ID, and hl hypothesis get root for the comments on the objective.

Examples:

  every hypothesis of the platform experiment. $pe is bound for you: pass no --param for
  it:
  `+queryExample+`

  text search on plain words, best matches first:
  hl hypothesis query 'CALL text_search.search_all("hypothesis_text", "learning rate warmup")
    YIELD node, score
    RETURN node.id, node.status, node.text ORDER BY score DESC LIMIT 10'

  the approaches, the children of the root:
  hl hypothesis query 'MATCH (h:Hypothesis)-[:BUILDS_ON]->(:Objective)
    RETURN h.id, h.status, h.text'

  one branch, everything that builds on a hypothesis at any depth:
  hl hypothesis query 'MATCH (d:Hypothesis)-[:BUILDS_ON*]->(:Hypothesis {id: $id})
    RETURN DISTINCT d.id, d.status, d.text' --param id=HYP_ID

  by job count, untested first:
  hl hypothesis query 'MATCH (h:Hypothesis)
    RETURN h.id, h.text, coalesce($evidence[h.id].jobs, 0) AS n ORDER BY n'`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment whose graph is read, bound as $pe (default the token's)")
	var params stringList
	fs.Var(&params, "param", "k=v, bound as $k (string value; repeatable)")
	paramsJSON := fs.String("params", "", "")
	full := fs.Bool("full", false, "every value whole, strings uncut")
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
	*pe = experimentOf(c, *pe)
	if *pe == "" {
		return parseExit(misuse(fs, "--platform-experiment is required: HL_TOKEN names none"))
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
		code := fail(stderr, err)
		var refused *apiclient.HTTPError
		if errors.As(err, &refused) && refused.Status == http.StatusBadRequest {
			fmt.Fprintf(stderr, "hl: hypothesis query: every node names its label and is tied to platform experiment %s for you ($pe, pass no --param for it); 'hl hypothesis query --help' has what a query may contain. A query that works:\n  %s\n", *pe, queryExample)
		}
		return code
	}
	limit, cut := textCut, 0
	if *full {
		limit = 0
	}
	for i, row := range out.Rows {
		if len(row) != len(out.Columns) {
			return fail(stderr, fmt.Errorf("row %d has %d values for %d columns", i, len(row), len(out.Columns)))
		}
		line, n, err := cutStrings(rowObject(out.Columns, row), limit)
		if err != nil {
			return fail(stderr, fmt.Errorf("response is not JSON: %w", err))
		}
		cut += n
		fmt.Fprintf(stdout, "%s\n", line)
	}
	if len(out.Rows) == 0 {
		fmt.Fprintln(stderr, "hl: hypothesis query: no rows")
	}
	if cut > 0 {
		fmt.Fprintf(stderr, "hl: hypothesis query: %d values cut to %d characters; --full prints them whole\n", cut, textCut)
	}
	if out.Truncated {
		fmt.Fprintf(stderr, "hl: hypothesis query: truncated at %d rows; narrow the query or add LIMIT/SKIP\n", len(out.Rows))
	}
	return 0
}

// textCut is how many characters of a string hl hypothesis list and query print unless told --full.
const textCut = 240

// cutStrings re-encodes raw on one line in its own key order, every string longer than limit
// runes cut to them and "…", none when limit is 0, and says how many it cut.
func cutStrings(raw []byte, limit int) ([]byte, int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b bytes.Buffer
	cut := 0
	var value func() error
	value = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			open, end := byte('{'), byte('}')
			if t == '[' {
				open, end = '[', ']'
			}
			b.WriteByte(open)
			for i := 0; dec.More(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				if open == '{' {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					writeString(&b, key.(string))
					b.WriteByte(':')
				}
				if err := value(); err != nil {
					return err
				}
			}
			b.WriteByte(end)
			_, err := dec.Token()
			return err
		case string:
			if r := []rune(t); limit > 0 && len(r) > limit {
				t = string(r[:limit]) + "…"
				cut++
			}
			writeString(&b, t)
		case json.Number:
			b.WriteString(t.String())
		case bool:
			b.WriteString(strconv.FormatBool(t))
		case nil:
			b.WriteString("null")
		}
		return nil
	}
	if err := value(); err != nil {
		return nil, 0, err
	}
	return b.Bytes(), cut, nil
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

// titleSlugLength is where the hypothesis's text is cut in a branch name.
const titleSlugLength = 40

// slug is text as part of a branch name: lowercase letters and digits, every run of anything
// else one dash, cut to limit when limit is positive.
func slug(text string, limit int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() == limit {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// implementationBranch names the branch one agent's code for one hypothesis lives on:
// hl/<platform experiment>/<agent name>/<start of the hypothesis's text>-<end of its id>. The
// end of the id tells apart two hypotheses that begin alike.
func implementationBranch(platformExperiment, agentName, text, hypothesisID string) (string, error) {
	agent, id := slug(agentName, 0), slug(strings.ReplaceAll(hypothesisID, "-", ""), 0)
	if agent == "" || len(id) < 6 {
		return "", fmt.Errorf("agent name %q and hypothesis id %q make no branch name", agentName, hypothesisID)
	}
	title := id[len(id)-6:]
	if words := slug(text, titleSlugLength); words != "" {
		title = words + "-" + title
	}
	return branchPrefix(platformExperiment) + agent + "/" + title, nil
}

func hypothesisCheckout(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("hypothesis checkout", `usage: hl hypothesis checkout ID [--from JOB]

Run in your clone of the platform experiment's repository. Puts it on your branch for
hypothesis ID: your implementation of it. A hypothesis is an idea and a branch is one agent's
code for it, so any agent may check out any hypothesis, its own or another's, and each gets a
branch of its own: hl/PE/<your agent name>/<start of the hypothesis text>-<end of its id>.

It fetches origin first. When your branch exists, here or on origin, it is checked out as it
is. Otherwise it is created: from the commit job JOB ran with --from JOB (any job of the
platform experiment, to start from one particular implementation), else from base_ref of the
hypothesis: the commit the newest completed job of the hypothesis it builds on ran, whichever
agent's, that hypothesis's own base while it has no completed job, and the baseline job's
commit for one that builds on the objective. To drop an idea and start again from the
baseline, register a hypothesis that builds on root and check it out; to improve an idea,
register one that builds on it.

The checkout is recorded as you working on the hypothesis: hl hypothesis get and list show you
in working and the coordinator sees it, for two hours or until a job of yours shows it.
Checking out again renews it. Another agent already on it is no refusal: two approaches to one
idea can both be worth running. Read what they do before you repeat it.

Prints one JSON object: branch; base, the commit a branch it created starts from; and, when
other agents are on the hypothesis now, working, who, and warning. Refused with the reason
when there is nothing to start from (no baseline is declared yet), and when --from is given for
a branch that exists. hl job submit pushes the branch.`, stderr)
	agent := fs.String("agent", "", "whose branch it is, for a person or an API key; an agent's token says it")
	from := fs.String("from", "", "create the branch from the commit this `job` ran")
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	if *agent = agentOf(c, *agent); *agent == "" {
		return parseExit(misuse(fs, "--agent is required: HL_TOKEN is no agent's token, so say whose branch this is"))
	}
	if id, err = fullHypothesisID(c, experimentOf(c, ""), id); err != nil {
		return fail(stderr, err)
	}
	var h struct {
		ID                 string `json:"id"`
		PlatformExperiment string `json:"platform_experiment_id"`
		Text               string `json:"text"`
		BaseRef            string `json:"base_ref"`
	}
	if err := c.Do("GET", "/hypotheses/"+url.PathEscape(id)+"?limit=1", nil, &h); err != nil {
		return fail(stderr, err)
	}
	var signup struct {
		Name string `json:"name"`
	}
	if err := c.Do("GET", signupPath(h.PlatformExperiment, *agent), nil, &signup); err != nil {
		return fail(stderr, err)
	}
	branch, err := implementationBranch(h.PlatformExperiment, signup.Name, h.Text, h.ID)
	if err != nil {
		return fail(stderr, fmt.Errorf("hypothesis checkout: %w", err))
	}
	base := h.BaseRef
	if *from != "" {
		var job struct {
			PlatformExperiment string `json:"platform_experiment_id"`
			CodeRef            string `json:"code_ref"`
		}
		if err := c.Do("GET", "/experiments/"+url.PathEscape(*from), nil, &job); err != nil {
			return fail(stderr, err)
		}
		if job.PlatformExperiment != h.PlatformExperiment {
			return fail(stderr, fmt.Errorf("hypothesis checkout: job %s is of platform experiment %s and hypothesis %s of %s: --from takes a job of the same one", *from, job.PlatformExperiment, h.ID, h.PlatformExperiment))
		}
		base = job.CodeRef
	}
	created, err := checkoutBranch("", branch, base, *from)
	if err != nil {
		return fail(stderr, fmt.Errorf("hypothesis checkout: %w", err))
	}
	out := struct {
		Branch  string            `json:"branch"`
		Base    string            `json:"base,omitempty"`
		Working []json.RawMessage `json:"working,omitempty"`
		Warning string            `json:"warning,omitempty"`
	}{Branch: branch, Base: created}
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(h.ID)+"/checkouts", map[string]string{"agent_id": *agent, "branch": branch}, &out); err != nil {
		fmt.Fprintf(stderr, "hl: hypothesis checkout: the branch is checked out and the platform did not record it: %v\n", err)
	}
	line, _ := json.Marshal(out)
	fmt.Fprintf(stdout, "%s\n", line)
	return 0
}

// checkoutBranch puts the clone in dir on branch after fetching origin. A branch the clone or
// origin has is checked out as it is, and from, the job a new one was to start from, is then
// refused. One neither has is created at the commit of base, a code_ref, and that commit is
// returned.
func checkoutBranch(dir, branch, base, from string) (created string, err error) {
	if _, err := git(dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", errors.New("this is not a git repository: run it in your clone of the platform experiment's repository (hl platform-experiments get names repository_url)")
	}
	if _, err := git(dir, "fetch", "--quiet", "origin"); err != nil {
		return "", err
	}
	_, localErr := git(dir, "rev-parse", "--quiet", "--verify", "refs/heads/"+branch)
	_, remoteErr := git(dir, "rev-parse", "--quiet", "--verify", "refs/remotes/origin/"+branch)
	if localErr == nil || remoteErr == nil {
		if from != "" {
			return "", fmt.Errorf("your branch %s exists, so nothing is created from job %s: leave --from out to check it out as it is", branch, from)
		}
		args := []string{"checkout", "--quiet", branch}
		if localErr != nil {
			args = []string{"checkout", "--quiet", "-b", branch, "--track", "origin/" + branch}
		}
		return "", switchBranch(dir, args...)
	}
	if base == "" {
		return "", errors.New("there is no commit to start from: the hypothesis builds up to the objective and no baseline is declared yet. Wait for the baseline, or name a job to start from with --from JOB")
	}
	sha := base[strings.LastIndex(base, "@")+1:]
	if err := switchBranch(dir, "checkout", "--quiet", "-b", branch, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// switchBranch runs the git checkout of args. Refused over changes in the clone, it says what to
// do about them in place of git's error.
func switchBranch(dir string, args ...string) error {
	_, err := git(dir, args...)
	if err == nil {
		return nil
	}
	if changes, statusErr := git(dir, "status", "--porcelain"); statusErr == nil && strings.TrimSpace(changes) != "" {
		return errors.New("git will not switch branches over the changes in your clone (git status lists them). " +
			"Commit them (git add -A && git commit) or stash them (git stash -u), then run hl hypothesis checkout again; " +
			"the branch starts from its base, and git cherry-pick or git stash pop brings your changes onto it")
	}
	return err
}

// shortID is the start of a hypothesis id, which is a UUID: what an agent copies from a listing.
var shortID = regexp.MustCompile(`^[0-9a-f][0-9a-f-]{3,34}$`)

// maxPrefixMatches is how many ids an ambiguous start lists.
const maxPrefixMatches = 10

// fullHypothesisID is the hypothesis of platform experiment pe whose id starts with id, as git
// takes the start of a commit: id itself when it is no such start or pe is unknown, refused when
// no hypothesis's id starts with it or several do, with the ones that do.
func fullHypothesisID(c *apiclient.Client, pe, id string) (string, error) {
	if pe == "" || !shortID.MatchString(id) {
		return id, nil
	}
	var out struct {
		Rows [][]string `json:"rows"`
	}
	query := map[string]any{
		"platform_experiment_id": pe,
		"cypher":                 fmt.Sprintf("MATCH (h:Hypothesis) WHERE h.id STARTS WITH $prefix RETURN h.id ORDER BY h.id LIMIT %d", maxPrefixMatches+1),
		"params":                 map[string]any{"prefix": id},
	}
	if err := c.Do("POST", "/hypotheses/query", query, &out); err != nil {
		return "", fmt.Errorf("find the hypothesis whose id starts with %s: %w", id, err)
	}
	var ids []string
	for _, row := range out.Rows {
		if len(row) == 1 {
			ids = append(ids, row[0])
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], nil
	case len(ids) == 0:
		return "", fmt.Errorf("no hypothesis of %s has an id that starts with %s: hl hypothesis list prints their ids", pe, id)
	case len(ids) > maxPrefixMatches:
		return "", fmt.Errorf("more than %d hypotheses of %s have an id that starts with %s, among them %s: give more of the id", maxPrefixMatches, pe, id, strings.Join(ids[:maxPrefixMatches], ", "))
	}
	return "", fmt.Errorf("%d hypotheses of %s have an id that starts with %s: %s. Give more of the id", len(ids), pe, id, strings.Join(ids, ", "))
}
