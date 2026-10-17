package shared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

const rootTarget = "root"

const placementHelp = "place the idea in the graph: --builds-on root for a fundamentally new approach to the objective, " +
	"or --builds-on ID for each hypothesis it refines, extends or combines " +
	"(find candidates with 'hl hypothesis query' or 'hl hypothesis list')"

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
per item. Full reference: $HL_REFERENCE/hypothesis.yaml`

func HypothesisSubmit(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis submit", submitUsage, stderr)
	agent := fs.String("agent", "", "agent whose hypothesis it is (default the token's agent, else the caller's own)")
	pe := fs.String("platform-experiment", "", "platform experiment id (default the token's)")
	hypothesisText := command.NewTextFlag(fs, "text", "hypothesis text")
	file := fs.String("file", "", "YAML file of one hypothesis, or a hypotheses: list of them (see --help)")
	var buildsOn, contradicts, related, distinctFrom idList
	fs.Var(&buildsOn, "builds-on", "hypothesis id this refines, extends or combines, or root for the objective (repeatable or comma-separated, at least one)")
	fs.Var(&contradicts, "contradicts", "hypothesis id that cannot hold together with this one (repeatable or comma-separated)")
	fs.Var(&related, "related", "hypothesis id to cross-reference (repeatable or comma-separated)")
	fs.Var(&distinctFrom, "distinct-from", "hypothesis id a refused submit listed that you judged a different claim (repeatable or comma-separated)")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	text, err := hypothesisText.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}

	items := []map[string]any{{}}
	if *file == "-" {
		if err := fs.ClaimStdin("--file -"); err != nil {
			return command.ParseExit(err)
		}
	}
	if *file != "" {
		raw, err := command.ReadYAMLItems(*file, "hypotheses")
		if err != nil {
			fmt.Fprintf(stderr, "hl: %s\n", err)
			return 2
		}
		items = raw
	}
	var flagLinks []platformapi.LinkSpec
	for _, group := range []struct {
		typ platformapi.LinkType
		ids idList
	}{{platformapi.LinkBuildsOn, buildsOn}, {platformapi.LinkContradicts, contradicts}, {platformapi.LinkRelated, related}} {
		for _, id := range group.ids {
			flagLinks = append(flagLinks, platformapi.LinkSpec{Type: group.typ, To: id})
		}
	}

	owner := c.Agent(*agent)
	bodies, keys, err := buildSubmitBodies(items, flagLinks, distinctFrom, map[string]string{
		"agent_id": owner, "platform_experiment_id": c.Experiment(*pe), "text": text,
	})
	if err != nil {
		return command.ParseExit(command.Misuse(fs, "%s", err))
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
			return apiclient.FullHypothesisID(c, body.PlatformExperimentID, ref)
		}
		for j := range body.Links {
			if body.Links[j].To, err = named(body.Links[j].To); err != nil {
				return command.Fail(stderr, err)
			}
		}
		for j := range body.DistinctFrom {
			if body.DistinctFrom[j], err = named(body.DistinctFrom[j]); err != nil {
				return command.Fail(stderr, err)
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
			return command.Fail(stderr, fmt.Errorf("%s%w", item, err))
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
			return command.Fail(stderr, fmt.Errorf("item %d: response carries no id: %s", i, out))
		}
		if keys[i] != "" {
			ids[keys[i]] = created.ID
		}
		if err := command.PrintLine(stdout, out); err != nil {
			return command.Fail(stderr, err)
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
	if reason, _ := command.RefusalOf(err); reason != platformapi.ReasonSimilarHypotheses || !errors.As(err, &refused) || json.Unmarshal([]byte(refused.Body), &reply) != nil {
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
		_ = command.PrintLine(stderr, raw)
	}
	fmt.Fprintf(stderr, "hl: hypothesis submit: judge each. The same claim as yours: register nothing and test it with "+
		"hl job submit FILE --hypothesis ID; its verdict stays its owner's. A different claim, narrower and broader ones "+
		"included: rerun adding --distinct-from %s, with --contradicts ID instead for one that cannot hold together with yours. "+
		"A refuted parent you build on stays your parent: keep its --builds-on and add it to --distinct-from when your claim "+
		"changes what refuted it.\n",
		strings.Join(ids, ","))
	return true
}

// buildSubmitBodies validates every item before anything is sent, so a bad item late in a file
// never leaves the earlier ones half submitted. Links to a local key stay as the key here and are
// swapped for the id the earlier item came back with at submit time.
func buildSubmitBodies(items []map[string]any, flagLinks []platformapi.LinkSpec, flagDistinct []string, overrides map[string]string) ([]*platformapi.RegisterHypothesisRequest, []string, error) {
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

	bodies := make([]*platformapi.RegisterHypothesisRequest, len(items))
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
		resolved := make([]platformapi.LinkSpec, 0, len(links))
		placed := false
		for _, l := range links {
			if !slices.Contains(platformapi.LinkTypes, l.Type) {
				return nil, nil, itemErr(items, i, fmt.Errorf("link type %q must be one of %s", l.Type, joined(platformapi.LinkTypes, ", ")))
			}
			if l.To == "" {
				return nil, nil, itemErr(items, i, fmt.Errorf("%s link has no target", l.Type))
			}
			if l.To == rootTarget {
				if l.Type != platformapi.LinkBuildsOn {
					return nil, nil, itemErr(items, i, fmt.Errorf("only builds_on may target root"))
				}
				l.To = fields["platform_experiment_id"]
			} else if at, ok := keyIndex[l.To]; ok && at >= i {
				return nil, nil, itemErr(items, i, fmt.Errorf("link to key %q, which item %d defines; list a parent before its children", l.To, at))
			}
			placed = placed || l.Type == platformapi.LinkBuildsOn
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
		bodies[i] = &platformapi.RegisterHypothesisRequest{
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

func parseLinks(v any) ([]platformapi.LinkSpec, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("links must be a list of {type, to}")
	}
	out := make([]platformapi.LinkSpec, 0, len(list))
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
		out = append(out, platformapi.LinkSpec{Type: platformapi.LinkType(typ), To: to})
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

// joined is values separated by sep.
func joined[T ~string](values []T, sep string) string {
	names := make([]string, len(values))
	for i, v := range values {
		names[i] = string(v)
	}
	return strings.Join(names, sep)
}

func HypothesisLink(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis link", `usage: hl hypothesis link ID --type builds_on|contradicts|related --to ID

Adds one link from your own hypothesis ID (--from ID works too), or from one no agent owns.
Links are append-only. Prints the updated outgoing links. A person or an API key linking from
an agent's hypothesis names that agent with --agent, and is kept as who made the link.`, stderr)
	agent := fs.String("agent", "", "agent whose hypothesis ID is (default the token's agent, else the caller's own)")
	typ := fs.String("type", "", "builds_on, contradicts or related")
	to := fs.String("to", "", "hypothesis id the link points at")
	from, err := command.ParseOne(fs, args, "ID", "", "id", "from")
	if err != nil {
		return command.ParseExit(err)
	}
	if *typ == "" || *to == "" {
		return command.ParseExit(command.Misuse(fs, "--type and --to are required"))
	}
	if !slices.Contains(platformapi.LinkTypes, platformapi.LinkType(*typ)) {
		return command.ParseExit(command.Misuse(fs, "--type must be one of %s, got %q", joined(platformapi.LinkTypes, ", "), *typ))
	}
	for _, id := range []*string{&from, to} {
		if *id, err = apiclient.FullHypothesisID(c, c.Experiment(""), *id); err != nil {
			return command.Fail(stderr, err)
		}
	}
	owner := c.Agent(*agent)
	body := map[string]string{"type": *typ, "to": *to}
	if owner != "" {
		body["agent_id"] = owner
	}
	var out json.RawMessage
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(from)+"/links", body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLine(stdout, out); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

func HypothesisComment(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis comment", `usage: hl hypothesis comment ID|root --text T | --text-file FILE|-

A note on any hypothesis with no job behind it: a verified fact, a dead end, a revision. root,
or the platform experiment's id, is the objective: a note for every approach, such as the
published results on the problem, which every agent of the platform experiment is told of.
It is written by whoever HL_TOKEN is. An agent's token writes its own agent's note; a person or
an API key writes their own, or an agent's with --agent.`, stderr)
	agent := fs.String("agent", "", "agent whose note it is (default the token's agent, else the caller's own)")
	pe := fs.String("platform-experiment", "", "the platform experiment whose objective root names (default the token's)")
	note := command.NewTextFlag(fs, "text", "the note")
	id, err := command.ParseOne(fs, args, "ID", "", "id")
	if err != nil {
		return command.ParseExit(err)
	}
	text, err := note.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if text == "" {
		return command.ParseExit(command.Misuse(fs, "--text or --text-file is required"))
	}
	id, objective, err := objectiveOr(fs, c, id, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	if !objective {
		if id, err = apiclient.FullHypothesisID(c, c.Experiment(*pe), id); err != nil {
			return command.Fail(stderr, err)
		}
	}
	owner := c.Agent(*agent)
	body := map[string]string{"text": text}
	if owner != "" {
		body["agent_id"] = owner
	}
	var out json.RawMessage
	if err := c.Do("POST", "/hypotheses/"+url.PathEscape(id)+"/comments", body, &out); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLine(stdout, out); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

func HypothesisStatus(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis status", `usage: hl hypothesis status ID open|inconclusive | confirmed|refuted --against CONTROL_JOB | confirmed|refuted --despite REASON

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
	despiteFlag := command.NewTextFlag(fs, "despite", "why a confirmed or refuted verdict holds without a repeated run and a control")
	operands, err := command.ParseOperands(fs, args, "id")
	if err != nil {
		return command.ParseExit(err)
	}
	despite, err := despiteFlag.Value(fs)
	if err != nil {
		return command.ParseExit(err)
	}
	if len(operands) == 2 && *status == "" {
		*status, operands = operands[1], operands[:1]
	}
	if len(operands) != 1 || *status == "" {
		return command.ParseExit(command.Misuse(fs, "want ID and a verdict"))
	}
	id, err := apiclient.FullHypothesisID(c, c.Experiment(""), operands[0])
	if err != nil {
		return command.Fail(stderr, err)
	}
	var out json.RawMessage
	owner := c.Agent(*agent)
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
		return command.Fail(stderr, err)
	}
	if err := command.PrintLine(stdout, out); err != nil {
		return command.Fail(stderr, err)
	}
	return 0
}

func HypothesisList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis list", `usage: hl hypothesis list [--agent A] [--status S] [--by people] [--untaken] [--limit N] [--offset N] [--full]

The open ideas of people first, those no agent has settled or built on yet, then the most recent,
one JSON object per line with builds_on, child_count, finding_count, comment_count and working:
the agents on it now, a job of theirs queued or running or a checkout within two hours. Each
text is cut to its first 240 characters ("…" ends one cut) and status_actor and
platform_experiment_id are left out unless --full; hl hypothesis get ID prints one whole. For
structure or text search use 'hl hypothesis query'. The branches a hypothesis was checked out
or run on are in hl hypothesis get ID, as implementations. A page is at most --limit rows; when more
match, a line on stderr says how many and the --offset of the next page.`, stderr)
	pe := fs.String("platform-experiment", "", "platform experiment id (default the token's)")
	agent := fs.String("agent", "", "only this agent's hypotheses")
	status := fs.String("status", "", "open, confirmed, refuted or inconclusive")
	by := fs.String("by", "", "people: only the hypotheses a person registered")
	untaken := fs.Bool("untaken", false, "only hypotheses no agent is working on")
	limit := fs.Int("limit", 0, "page size (server default 20, max 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	full := fs.Bool("full", false, "every row whole: texts uncut, status_actor and platform_experiment_id")
	if err := fs.Parse(args); err != nil {
		return command.ParseExit(err)
	}
	if fs.NArg() > 0 {
		return command.ParseExit(command.Misuse(fs, "unexpected argument %q", fs.Arg(0)))
	}
	if *by != "" && *by != "people" {
		return command.ParseExit(command.Misuse(fs, "--by takes people, not %q", *by))
	}
	*pe = c.Experiment(*pe)
	if *pe == "" {
		return command.ParseExit(command.Misuse(fs, "--platform-experiment is required"))
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
	if *untaken {
		q.Set("untaken", "true")
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
		return command.Fail(stderr, err)
	}
	cut := 0
	for _, h := range out {
		var drop []string
		if !*full {
			n := 0
			if h, n, err = cutStrings(h, textCut); err != nil {
				return command.Fail(stderr, fmt.Errorf("response is not JSON: %w", err))
			}
			cut += n
			drop = []string{"status_actor", "platform_experiment_id"}
		}
		if err := command.PrintLine(stdout, h, drop...); err != nil {
			return command.Fail(stderr, err)
		}
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "hl: hypothesis list: no hypothesis matches")
	}
	if cut > 0 {
		fmt.Fprintf(stderr, "hl: hypothesis list: %d texts cut to %d characters; hl hypothesis get ID or --full prints them whole\n", cut, textCut)
	}
	command.PageNotice(stderr, "hypothesis list", header, *offset, len(out))
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
func objectiveOr(fs *command.Flags, c *apiclient.Client, id, flagValue string) (string, bool, error) {
	if id != rootTarget {
		pe := c.Experiment(flagValue)
		return id, id == pe || strings.HasPrefix(id, "pe-"), nil
	}
	pe, err := command.NamedExperiment(fs, c, flagValue)
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
		return command.Fail(stderr, err)
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
		return command.Fail(stderr, err)
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

func HypothesisGet(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis get", `usage: hl hypothesis get ID|root [--full] [--limit N]

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
	id, err := command.ParseOne(fs, args, "ID", "", "id")
	if err != nil {
		return command.ParseExit(err)
	}
	id, objective, err := objectiveOr(fs, c, id, *pe)
	if err != nil {
		return command.ParseExit(err)
	}
	if objective {
		return objectiveGet(c, id, *full, *limit, stdout, stderr)
	}
	if id, err = apiclient.FullHypothesisID(c, c.Experiment(*pe), id); err != nil {
		return command.Fail(stderr, err)
	}
	path := "/hypotheses/" + url.PathEscape(id)
	if *limit != 0 {
		path += "?limit=" + strconv.Itoa(*limit)
	}
	var out json.RawMessage
	if err := c.Do("GET", path, nil, &out); err != nil {
		return command.Fail(stderr, err)
	}
	var h struct {
		hypothesisBrief
		Jobs         []json.RawMessage `json:"jobs"`
		JobCount     int               `json:"job_count"`
		CommentCount int               `json:"comment_count"`
	}
	if err := json.Unmarshal(out, &h); err != nil {
		return command.Fail(stderr, fmt.Errorf("response is not JSON: %w", err))
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
		if brief, _ = command.CompactJSON(brief); len(brief) > 2 {
			lists = append(brief[1:len(brief)-1], ',')
		}
	}
	jobs, err := render(h.Jobs)
	if err != nil {
		return command.Fail(stderr, err)
	}
	line, err := command.CompactJSON(out, drop...)
	if err != nil {
		return command.Fail(stderr, fmt.Errorf("response is not JSON: %w", err))
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

func HypothesisQuery(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("hypothesis query", `usage: hl hypothesis query 'CYPHER' [--param k=v ...]
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
	var params command.StringList
	fs.Var(&params, "param", "k=v, bound as $k (string value; repeatable)")
	paramsJSON := fs.String("params", "", "")
	full := fs.Bool("full", false, "every value whole, strings uncut")
	cypher, err := command.ParseOne(fs, args, "query", "", "cypher")
	if err != nil {
		return command.ParseExit(err)
	}
	if *paramsJSON != "" {
		fmt.Fprintln(stderr, "hl: hypothesis query: $pe and $evidence are already bound; pass any other parameter as --param k=v")
		return 2
	}
	if cypher == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return command.Fail(stderr, fmt.Errorf("reading stdin: %w", err))
		}
		cypher = string(raw)
	}
	if strings.TrimSpace(cypher) == "" {
		return command.ParseExit(command.Misuse(fs, "empty query"))
	}
	if strings.HasPrefix(strings.TrimSpace(cypher), "{") {
		return command.ParseExit(command.Misuse(fs, "want the Cypher text itself, got a JSON object"))
	}
	*pe = c.Experiment(*pe)
	if *pe == "" {
		return command.ParseExit(command.Misuse(fs, "--platform-experiment is required: HL_TOKEN names none"))
	}
	bound := map[string]any{}
	for _, p := range params {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return command.ParseExit(command.Misuse(fs, "--param wants k=v, got %q", p))
		}
		bound[k] = v
	}
	var out struct {
		Columns   []string            `json:"columns"`
		Rows      [][]json.RawMessage `json:"rows"`
		Truncated bool                `json:"truncated"`
	}
	if err := c.Do("POST", "/hypotheses/query", map[string]any{"platform_experiment_id": *pe, "cypher": cypher, "params": bound}, &out); err != nil {
		code := command.Fail(stderr, err)
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
			return command.Fail(stderr, fmt.Errorf("row %d has %d values for %d columns", i, len(row), len(out.Columns)))
		}
		line, n, err := cutStrings(rowObject(out.Columns, row), limit)
		if err != nil {
			return command.Fail(stderr, fmt.Errorf("response is not JSON: %w", err))
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
					command.WriteJSONString(&b, key.(string))
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
			command.WriteJSONString(&b, t)
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
