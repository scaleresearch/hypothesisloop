package graphdb

import (
	"fmt"
	"slices"
	"strings"
)

// scopeKeys is the property that ties a node to its platform experiment, by label.
var scopeKeys = map[string]string{"Hypothesis": "platform_experiment_id", "Objective": "id"}

// queryStarts are the clauses a query may open with, which leaves out every statement that is
// no graph read (SHOW, USE, EXPLAIN, USING).
var queryStarts = []string{"MATCH", "OPTIONAL", "WITH", "UNWIND", "RETURN", "CALL"}

// QueryFunctions are the functions a read query may call, all of which compute from their arguments
// alone. A name outside it is refused, so a function of a query module never runs.
var QueryFunctions = []string{
	"count", "sum", "avg", "min", "max", "collect",
	"coalesce", "exists", "all", "any", "none", "single", "reduce", "extract",
	"size", "length", "head", "last", "tail", "range", "reverse", "isempty",
	"id", "labels", "type", "properties", "keys", "nodes", "relationships", "startnode", "endnode",
	"degree", "indegree", "outdegree",
	"tostring", "tointeger", "tofloat", "toboolean", "tolower", "toupper", "trim", "ltrim", "rtrim",
	"replace", "split", "substring", "left", "right", "startswith", "endswith", "contains",
	"abs", "ceil", "floor", "round", "sqrt", "sign", "exp", "log", "log2", "log10",
	"datetime", "date", "localtime", "localdatetime", "duration", "timestamp",
}

// groupKeywords are the keywords an opening parenthesis may follow. Any other word before one is
// taken for a function name and held to QueryFunctions.
var groupKeywords = []string{
	"MATCH", "WHERE", "AND", "OR", "XOR", "NOT", "RETURN", "WITH", "DISTINCT", "IN", "BY",
	"CASE", "WHEN", "THEN", "ELSE", "UNWIND", "LIMIT", "SKIP", "CONTAINS",
}

// hopScope is the filter put on every variable-length relationship, whose inner nodes have no
// node pattern of their own.
const hopScope = " (hl_scope_edge, hl_scope_node | (hl_scope_node:Hypothesis AND hl_scope_node.platform_experiment_id = $pe)" +
	" OR (hl_scope_node:Objective AND hl_scope_node.id = $pe))"

const scopeHelp = "a query reads one platform experiment: every node is written (v:Hypothesis) or (v:Objective), which the platform ties to $pe"

type insertion struct {
	at   int
	text string
}

// scoped returns cypher rewritten so that it reads the platform experiment bound as $pe and
// nothing else, or refuses it. A query reaches the graph in three ways and each is tied to $pe:
//
//   - a node pattern must name exactly one label, Hypothesis or Objective, and gets that label's
//     scope property set to $pe in its property map;
//   - a variable-length relationship gets a filter that holds every node it passes to $pe;
//   - a procedure call must YIELD node and is followed by a filter on it.
//
// Everything else a query may contain computes from what those bound. To keep that true, a
// function outside QueryFunctions and a statement outside queryStarts are refused. A parenthesis
// is read as a node pattern whenever Memgraph could read it as one, so what is refused for
// lacking a label may be a parenthesised variable: the price of never missing a pattern.
func scoped(cypher string) (string, error) {
	rs := []rune(cypher)
	toks, err := tokenize(rs)
	if err != nil {
		return "", err
	}
	if len(toks) == 0 || !slices.ContainsFunc(queryStarts, func(w string) bool { return isKeyword(toks, 0, w) }) {
		return "", fmt.Errorf("%w: a query starts with %s", ErrInvalidQuery, strings.Join(queryStarts, ", "))
	}
	if err := checkReadOnly(toks); err != nil {
		return "", err
	}
	closes, err := brackets(toks)
	if err != nil {
		return "", err
	}
	s := scoper{toks: toks, closes: closes, procedureArgs: map[int]bool{}}
	for i, t := range toks {
		switch {
		case t.quoted:
		case isKeyword(toks, i, "CALL") && !s.is(i+1, "{"):
			err = s.procedure(i)
		case t.text == "(":
			err = s.parenthesis(i)
		case t.text == "[" && s.is(i-1, "-"):
			err = s.relationship(i)
		}
		if err != nil {
			return "", err
		}
	}
	slices.SortStableFunc(s.insertions, func(a, b insertion) int { return a.at - b.at })
	var out strings.Builder
	at := 0
	for _, ins := range s.insertions {
		out.WriteString(string(rs[at:ins.at]))
		out.WriteString(ins.text)
		at = ins.at
	}
	out.WriteString(string(rs[at:]))
	return out.String(), nil
}

// brackets returns, for every opening bracket token, the index of the token that closes it.
func brackets(toks []token) (map[int]int, error) {
	pairs := map[string]string{")": "(", "]": "[", "}": "{"}
	closes := map[int]int{}
	var open []int
	for i, t := range toks {
		if t.quoted {
			continue
		}
		switch t.text {
		case "(", "[", "{":
			open = append(open, i)
		case ")", "]", "}":
			if len(open) == 0 || toks[open[len(open)-1]].text != pairs[t.text] {
				return nil, fmt.Errorf("%w: unbalanced %s", ErrInvalidQuery, t.text)
			}
			closes[open[len(open)-1]] = i
			open = open[:len(open)-1]
		}
	}
	if len(open) > 0 {
		return nil, fmt.Errorf("%w: unclosed %s", ErrInvalidQuery, toks[open[0]].text)
	}
	return closes, nil
}

type scoper struct {
	toks          []token
	closes        map[int]int
	procedureArgs map[int]bool
	insertions    []insertion
}

// is reports whether the token at i is the punctuation or the word text, as written.
func (s *scoper) is(i int, text string) bool {
	return i >= 0 && i < len(s.toks) && !s.toks[i].quoted && s.toks[i].text == text
}

func (s *scoper) name(i int) bool {
	return i >= 0 && i < len(s.toks) && (s.toks[i].quoted || isWord(s.toks[i].text))
}

func (s *scoper) word(i int) bool {
	return s.name(i) && !s.toks[i].quoted
}

// procedure scopes the CALL at i: CALL name(args) YIELD node [AS v][, score [AS w]].
func (s *scoper) procedure(i int) error {
	shape := fmt.Errorf("%w: a procedure is called as CALL name(...) YIELD node, score: its node is what the platform ties to $pe", ErrInvalidQuery)
	_, args := procedureName(s.toks, i+1)
	if !s.is(args, "(") {
		return shape
	}
	s.procedureArgs[args] = true
	at := s.closes[args] + 1
	if !isKeyword(s.toks, at, "YIELD") {
		return shape
	}
	node := ""
	for {
		at++
		if !s.word(at) {
			return shape
		}
		field, variable := s.toks[at].text, s.toks[at].text
		if isKeyword(s.toks, at+1, "AS") {
			at += 2
			if !s.word(at) {
				return shape
			}
			variable = s.toks[at].text
		}
		if field == "node" {
			node = variable
		}
		if !s.is(at+1, ",") {
			break
		}
		at++
	}
	if node == "" {
		return shape
	}
	// The second WITH lets the caller's own WHERE follow.
	s.insertions = append(s.insertions, insertion{s.toks[at].end, fmt.Sprintf(" WITH * WHERE %s.platform_experiment_id = $pe WITH *", node)})
	return nil
}

// parenthesis scopes the one opened at i when it is a node pattern.
func (s *scoper) parenthesis(i int) error {
	if s.procedureArgs[i] {
		return nil
	}
	if s.name(i - 1) {
		word := s.toks[i-1]
		dotted := word.quoted || s.is(i-2, ".")
		if !dotted && slices.Contains(QueryFunctions, strings.ToLower(word.text)) {
			return nil
		}
		if !dotted && (strings.EqualFold(word.text, "CREATE") || strings.EqualFold(word.text, "MERGE")) {
			return fmt.Errorf("%w: this endpoint is read-only; register hypotheses with POST /hypotheses and link them with POST /hypotheses/{id}/links", ErrInvalidQuery)
		}
		if dotted || !slices.Contains(groupKeywords, strings.ToUpper(word.text)) {
			return fmt.Errorf("%w: function %q is not available; a query may call only %s", ErrInvalidQuery, word.text, strings.Join(QueryFunctions, ", "))
		}
	}
	end := s.closes[i]
	at := i + 1
	if s.name(at) {
		if isKeyword(s.toks, at, "WHERE") || isKeyword(s.toks, at+1, "WHERE") ||
			(isKeyword(s.toks, at+1, "IS") && !isKeyword(s.toks, at+2, "NULL") && !isKeyword(s.toks, at+2, "NOT")) {
			return fmt.Errorf("%w: a node pattern takes a label and a property map only, put its condition in WHERE after the pattern", ErrInvalidQuery)
		}
		if !s.is(at+1, ")") && !s.is(at+1, ":") && !s.is(at+1, "{") && !s.is(at+1, "$") {
			return nil
		}
		at++
	} else if at != end && !s.is(at, ":") && !s.is(at, "{") && !s.is(at, "$") {
		return nil
	}
	pattern := fmt.Errorf("%w: %s; no other label, no second label and no parameter as the property map", ErrInvalidQuery, scopeHelp)
	if !s.is(at, ":") || !s.name(at+1) {
		return pattern
	}
	key, ok := scopeKeys[s.toks[at+1].text]
	if !ok {
		return pattern
	}
	at += 2
	if at == end {
		s.insertions = append(s.insertions, insertion{s.toks[end].start, fmt.Sprintf(" {%s: $pe}", key)})
		return nil
	}
	if !s.is(at, "{") || s.closes[at]+1 != end {
		return pattern
	}
	return s.properties(at, key)
}

// properties ties the property map opened at i to $pe on key, which the caller may have written
// as exactly that already.
func (s *scoper) properties(i int, key string) error {
	end := s.closes[i]
	stated := false
	for at := i + 1; at < end; at++ {
		if next, ok := s.closes[at]; ok {
			at = next
			continue
		}
		if !s.is(at-1, "{") && !s.is(at-1, ",") {
			continue
		}
		if !s.name(at) || s.toks[at].text != key || !s.is(at+1, ":") {
			continue
		}
		if !s.is(at+2, "$") || !s.is(at+3, "pe") || (at+4 != end && !s.is(at+4, ",")) {
			return fmt.Errorf("%w: %s is $pe on every node, which the platform sets: leave it out or write %s: $pe", ErrInvalidQuery, key, key)
		}
		stated = true
	}
	if stated {
		return nil
	}
	text := key + ": $pe"
	if i+1 != end {
		text += ", "
	}
	s.insertions = append(s.insertions, insertion{s.toks[i].end, text})
	return nil
}

// relationship scopes the relationship detail opened at i when it is of variable length.
func (s *scoper) relationship(i int) error {
	end := s.closes[i]
	variable, lambda := false, false
	for at := i + 1; at < end; at++ {
		variable = variable || s.is(at, "*")
		lambda = lambda || s.is(at, "(")
		if next, ok := s.closes[at]; ok {
			at = next
		}
	}
	if !variable {
		return nil
	}
	if lambda {
		return fmt.Errorf("%w: a variable-length relationship takes its type, bounds and property map only ([:BUILDS_ON*1..3]); the platform adds the filter that keeps it in $pe", ErrInvalidQuery)
	}
	s.insertions = append(s.insertions, insertion{s.toks[end].start, hopScope})
	return nil
}
