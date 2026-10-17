package graphdb

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

const (
	// QueryTimeout is how long one read query may run on the server.
	QueryTimeout = 5 * time.Second
	// QueryRowCap is the most rows one read query returns; more sets QueryResult.Truncated.
	QueryRowCap = 200
	// QueryMemoryLimitMB is how much memory one read query may allocate on the server.
	QueryMemoryLimitMB = 256
)

// QueryProcedures are the only procedures a read query may CALL, each yielding the hypotheses it
// found as node. Every other procedure is refused because some (mg.create_module_file and
// friends) write to the database host outside any transaction, so rolling back would not undo
// them, and because one that yields no node (text_search.aggregate) reads across platform
// experiments with nothing to scope.
var QueryProcedures = []string{
	"text_search.search",
	"text_search.search_all",
	"text_search.regex_search",
	"text_search.fuzzy_phrase_search",
}

// QueryResult is a read query's answer, rows in the order Memgraph returned them.
type QueryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

// Query runs one read-only Cypher query over one platform experiment's graph, in an explicit
// read-mode transaction that is always rolled back. Memgraph refuses a write clause in a read-mode
// transaction and refuses index, constraint, user, trigger and storage statements in any explicit
// one; the rollback is the backstop. What neither covers (procedures that write outside the
// transaction, LOAD CSV) is refused before the query is sent (see checkReadOnly). params are JSON
// values: a whole number arrives as float64 and is passed on as an integer so it works in LIMIT and
// comparisons.
//
// The platform experiment is bound as $pe and the query is rewritten before it is sent so that every
// place it can read the graph is tied to that platform experiment (see scoped): what it returns
// is never inspected for where it came from.
//
// Nodes come back as their properties plus _labels, relationships as their properties plus _type,
// _from and _to (the endpoints' id properties), paths as a list of those, scalars as they are.
func (s *Store) Query(ctx context.Context, platformExperimentID, cypher string, params map[string]any) (*QueryResult, error) {
	if platformExperimentID == "" {
		return nil, fmt.Errorf("%w: a query reads one platform experiment, name it", ErrInvalidQuery)
	}
	if _, ok := params["pe"]; ok {
		return nil, fmt.Errorf("%w: $pe is already bound to the platform experiment: use $pe in the query without passing it, and name your own parameter differently", ErrInvalidQuery)
	}
	cypher, err := scoped(cypher)
	if err != nil {
		return nil, err
	}
	bound := jsonParams(params)
	bound["pe"] = platformExperimentID
	ctx, cancel := context.WithTimeout(ctx, 2*QueryTimeout)
	defer cancel()
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	tx, err := session.BeginTransaction(ctx, neo4j.WithTxTimeout(QueryTimeout))
	if err != nil {
		return nil, fmt.Errorf("graphdb.Query: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	result, err := tx.Run(ctx, withMemoryLimit(cypher), bound)
	if err != nil {
		return nil, queryError(err)
	}
	columns, err := result.Keys()
	if err != nil {
		return nil, queryError(err)
	}
	out := &QueryResult{Columns: columns, Rows: [][]any{}}
	var raw [][]any
	for result.Next(ctx) {
		if len(raw) == QueryRowCap {
			out.Truncated = true
			break
		}
		raw = append(raw, result.Record().Values)
	}
	// Discards whatever the cap left unread. With the stream still open, the next query in this
	// transaction makes the driver pull every remaining record into memory first.
	if _, err := result.Consume(ctx); err != nil {
		return nil, queryError(err)
	}
	nodes, err := resultNodes(ctx, tx, raw)
	if err != nil {
		return nil, queryError(err)
	}
	var d decoder
	ids := make(map[string]string, len(nodes))
	for id, n := range nodes {
		ids[id] = d.str(n.Props["id"])
	}
	if d.err != nil {
		return nil, fmt.Errorf("graphdb.Query: %w", d.err)
	}
	for _, values := range raw {
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = mapValue(v, ids)
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// withMemoryLimit appends Memgraph's QUERY MEMORY LIMIT clause on a new line, so a trailing line
// comment cannot swallow it. Trailing semicolons are dropped because Memgraph refuses the clause
// after one. A query that sets its own limit is refused for having two.
func withMemoryLimit(cypher string) string {
	cypher = strings.TrimRight(cypher, "; \t\r\n")
	return fmt.Sprintf("%s\nQUERY MEMORY LIMIT %d MB", cypher, QueryMemoryLimitMB)
}

func queryError(err error) error {
	var ne *neo4j.Neo4jError
	if errors.As(err, &ne) && (strings.Contains(ne.Code, "ClientError") || strings.Contains(ne.Code, "TransientError")) {
		msg := ne.Msg
		if strings.Contains(msg, "Accessor type READ") {
			msg = "this endpoint is read-only; register hypotheses with POST /hypotheses and link them with POST /hypotheses/{id}/links (" + msg + ")"
		}
		if strings.Contains(msg, "timeout") {
			msg = fmt.Sprintf("%s (queries are limited to %s)", msg, QueryTimeout)
		}
		if strings.Contains(msg, "Memory limit exceeded") {
			msg = fmt.Sprintf("%s (queries are limited to %d MB)", msg, QueryMemoryLimitMB)
		}
		return fmt.Errorf("%w: %s", ErrInvalidQuery, msg)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the query did not finish within %s", ErrInvalidQuery, 2*QueryTimeout)
	}
	return fmt.Errorf("graphdb.Query: %w", err)
}

func jsonParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = jsonValue(v)
	}
	return out
}

func jsonValue(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
		return t
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jsonValue(e)
		}
		return out
	case map[string]any:
		return jsonParams(t)
	default:
		return v
	}
}

// resultNodes returns every node in rows by element id, including relationship endpoints, which
// the driver reports by element id only and are fetched in the same transaction to name them.
func resultNodes(ctx context.Context, tx neo4j.ExplicitTransaction, rows [][]any) (map[string]neo4j.Node, error) {
	nodes := map[string]neo4j.Node{}
	var endpoints []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case neo4j.Node:
			nodes[t.ElementId] = t
		case neo4j.Relationship:
			endpoints = append(endpoints, t.StartElementId, t.EndElementId)
		case neo4j.Path:
			for _, n := range t.Nodes {
				walk(n)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	for _, row := range rows {
		for _, v := range row {
			walk(v)
		}
	}
	var lookup []string
	for _, id := range endpoints {
		if _, ok := nodes[id]; !ok {
			lookup = append(lookup, id)
		}
	}
	if len(lookup) == 0 {
		return nodes, nil
	}
	result, err := tx.Run(ctx, `MATCH (n) WHERE elementId(n) IN $ids RETURN n`, map[string]any{"ids": lookup})
	if err != nil {
		return nil, err
	}
	records, err := result.Collect(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		n := r.Values[0].(neo4j.Node)
		nodes[n.ElementId] = n
	}
	return nodes, nil
}

func mapValue(v any, ids map[string]string) any {
	switch t := v.(type) {
	case neo4j.Node:
		m := mapProps(t.Props, ids)
		m["_labels"] = t.Labels
		return m
	case neo4j.Relationship:
		m := mapProps(t.Props, ids)
		m["_type"] = t.Type
		m["_from"] = ids[t.StartElementId]
		m["_to"] = ids[t.EndElementId]
		return m
	case neo4j.Path:
		out := make([]any, 0, len(t.Nodes)+len(t.Relationships))
		for i, n := range t.Nodes {
			out = append(out, mapValue(n, ids))
			if i < len(t.Relationships) {
				out = append(out, mapValue(t.Relationships[i], ids))
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = mapValue(e, ids)
		}
		return out
	case map[string]any:
		return mapProps(t, ids)
	case time.Time:
		// Memgraph's datetime() takes 3 or 6 fraction digits, so a value read here can be
		// compared with datetime() as is; RFC3339Nano trims trailing zeros.
		return t.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	default:
		return v
	}
}

func mapProps(props map[string]any, ids map[string]string) map[string]any {
	out := make(map[string]any, len(props)+3)
	for k, v := range props {
		out[k] = mapValue(v, ids)
	}
	return out
}

// checkReadOnly refuses the statements an always-rolled-back transaction cannot contain: LOAD
// (CSV, PARQUET and JSONL each read files on the database host), USING PERIODIC COMMIT and CALL {...} IN TRANSACTIONS (commit
// batches on their own, verified to persist through a rollback), and any CALL of a procedure
// outside QueryProcedures. It works on tokens with strings and comments removed, so a keyword
// inside a string literal or a comment is not mistaken for one in the query.
func checkReadOnly(toks []token) error {
	keyword := func(i int, word string) bool { return isKeyword(toks, i, word) }
	for i := range toks {
		switch {
		case keyword(i, "LOAD"):
			return fmt.Errorf("%w: LOAD is not available, this endpoint reads the hypothesis graph only", ErrInvalidQuery)
		case keyword(i, "USING") && keyword(i+1, "PERIODIC"),
			keyword(i, "IN") && (keyword(i+1, "TRANSACTIONS") || keyword(i+1, "CONCURRENT")):
			return fmt.Errorf("%w: USING PERIODIC COMMIT and CALL {...} IN TRANSACTIONS are not available, this endpoint is read-only", ErrInvalidQuery)
		case keyword(i, "CALL"):
			if i+1 < len(toks) && !toks[i+1].quoted && toks[i+1].text == "{" {
				continue
			}
			name, _ := procedureName(toks, i+1)
			if !slices.Contains(QueryProcedures, name) {
				return fmt.Errorf("%w: procedure %q is not available; a query may CALL only %s", ErrInvalidQuery, name, strings.Join(QueryProcedures, ", "))
			}
		}
	}
	return nil
}

// isKeyword reports whether the token at i is the keyword word. A word right after a dot is a
// property or a name part, never a keyword.
func isKeyword(toks []token, i int, word string) bool {
	return i < len(toks) && !toks[i].quoted && strings.EqualFold(toks[i].text, word) &&
		(i == 0 || toks[i-1].quoted || toks[i-1].text != ".")
}

// procedureName reads a dotted procedure name from the tokens at i, after CALL, joining
// backtick-quoted parts the way Memgraph resolves them (`mg`.`load` and `mg.load` both name
// mg.load), and returns the index of the token after it.
func procedureName(toks []token, i int) (string, int) {
	var parts []string
	expectName := true
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case expectName && (t.quoted || isWord(t.text)):
			parts = append(parts, t.text)
			expectName = false
		case !expectName && !t.quoted && t.text == ".":
			expectName = true
		default:
			return strings.ToLower(strings.Join(parts, ".")), i
		}
	}
	return strings.ToLower(strings.Join(parts, ".")), i
}

// token is one lexeme of a query; start and end are its rune offsets in the text.
type token struct {
	text       string
	quoted     bool
	start, end int
}

func isWordRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isWord(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return !isWordRune(r) })
}

// plainText returns the first rune of rs that is neither printable ASCII nor a tab or a line
// break as a refusal. Memgraph reads more runes as whitespace, name parts and dashes than any
// list kept here would hold, so outside string literals and backtick names a query has none.
func plainText(rs []rune) error {
	for _, r := range rs {
		if r != '\t' && r != '\n' && r != '\r' && (r < ' ' || r > '~') {
			return fmt.Errorf("%w: outside string literals and backtick names a query is printable ASCII, and this one holds %U", ErrInvalidQuery, r)
		}
	}
	return nil
}

// tokenize splits cypher into words, backtick-quoted names and single punctuation characters,
// dropping whitespace and comments and reducing each string literal to its opening quote. A line
// comment ends at either line break character, so nothing Memgraph could read as query text is
// dropped as a comment.
func tokenize(rs []rune) ([]token, error) {
	var toks []token
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			i++
		case r == '/' && i+1 < len(rs) && rs[i+1] == '/':
			j := i
			for j < len(rs) && rs[j] != '\n' && rs[j] != '\r' {
				j++
			}
			if err := plainText(rs[i:j]); err != nil {
				return nil, err
			}
			i = j
		case r == '/' && i+1 < len(rs) && rs[i+1] == '*':
			j := i + 2
			for j+1 < len(rs) && (rs[j] != '*' || rs[j+1] != '/') {
				j++
			}
			if j+1 >= len(rs) {
				return nil, fmt.Errorf("%w: unterminated /* comment", ErrInvalidQuery)
			}
			if err := plainText(rs[i:j]); err != nil {
				return nil, err
			}
			i = j + 2
		case r == '\'' || r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != r {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("%w: unterminated string literal", ErrInvalidQuery)
			}
			toks = append(toks, token{text: string(r), start: i, end: j + 1})
			i = j + 1
		case r == '`':
			var name strings.Builder
			j := i + 1
			for {
				if j >= len(rs) {
					return nil, fmt.Errorf("%w: unterminated backtick name", ErrInvalidQuery)
				}
				if rs[j] == '`' {
					if j+1 < len(rs) && rs[j+1] == '`' {
						name.WriteRune('`')
						j += 2
						continue
					}
					break
				}
				name.WriteRune(rs[j])
				j++
			}
			toks = append(toks, token{text: name.String(), quoted: true, start: i, end: j + 1})
			i = j + 1
		case isWordRune(r):
			j := i
			for j < len(rs) && isWordRune(rs[j]) {
				j++
			}
			toks = append(toks, token{text: string(rs[i:j]), start: i, end: j})
			i = j
		case r == '$' && i > 0 && isWordRune(rs[i-1]):
			return nil, fmt.Errorf("%w: write a space before $: Memgraph reads a $ that follows a name as part of that name", ErrInvalidQuery)
		default:
			if err := plainText(rs[i : i+1]); err != nil {
				return nil, err
			}
			toks = append(toks, token{text: string(r), start: i, end: i + 1})
			i++
		}
	}
	return toks, nil
}
