package graphdb

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// The guard is what stands between a rolled-back read and the statements a rollback cannot undo,
// so it is checked here without a database too: it must not be fooled by quoting, comments or case,
// and must not refuse an ordinary read that merely mentions those words.
func TestCheckReadOnly(t *testing.T) {
	for q, refused := range map[string]bool{
		"MATCH (h:Hypothesis) RETURN h.id":                                                      false,
		"CALL text_search.search('hypothesis_text', 'data.text:x') YIELD node RETURN node":      false,
		"CALL `text_search`.`search_all`('hypothesis_text', 'x') YIELD node RETURN node":        false,
		"MATCH (n:Hypothesis) CALL { WITH n RETURN n.id AS id } RETURN id":                      false,
		"RETURN 'LOAD CSV, USING PERIODIC COMMIT, CALL mg.load()' AS s":                         false,
		"MATCH (n:Hypothesis) WHERE n.call = 1 OR n.load = 2 RETURN n /* CALL mg.load_all() */": false,
		"call mg.load_all()":                                                              true,
		"CALL `mg.create_module_file`('a.py', '')":                                        true,
		"CALL mg//x\n.create_module_file('a.py', '')":                                     true,
		"CALL text_search.search_edges('i', 'x')":                                         true,
		"CALL text_search.aggregate('i', 'x', '{}') YIELD aggregation RETURN aggregation": true,
		"LoAd CsV FROM '/etc/passwd' AS r RETURN r":                                       true,
		"USING periodic COMMIT 1 MATCH (n) RETURN n":                                      true,
		"CALL { CREATE (:X) } IN TRANSACTIONS OF 1 ROWS":                                  true,
		"CALL { CREATE (:X) } IN CONCURRENT TRANSACTIONS":                                 true,
		"RETURN 'unterminated":                                                            true,
		"MATCH (n) RETURN n /* unterminated":                                              true,
	} {
		_, err := scoped(q)
		if got := errors.Is(err, ErrInvalidQuery); got != refused {
			t.Errorf("scoped(%q): got = %v, want refused = %v", q, err, refused)
		}
	}
}

// A datetime read from a query must be accepted back by datetime(), which wants 3 or 6 fraction
// digits: RFC3339Nano would print .12345 for a time whose last microsecond digit is zero.
func TestQueryDatetimesKeepSixFractionDigits(t *testing.T) {
	at := time.Date(2026, 10, 2, 8, 59, 0, 123450000, time.FixedZone("x", 3600))
	if got, want := mapValue(at, nil), "2026-10-02T07:59:00.123450Z"; got != want {
		t.Fatalf("mapValue(%v) = %v, want %s", at, got, want)
	}
}

// Every way a query reaches the graph is tied to $pe in the text that is sent, so what it
// returns needs no inspection.
func TestAQueryIsRewrittenToItsPlatformExperiment(t *testing.T) {
	for q, want := range map[string]string{
		"MATCH (h:Hypothesis) RETURN h.id, h.text LIMIT 25":                                                                          "MATCH (h:Hypothesis {platform_experiment_id: $pe}) RETURN h.id, h.text LIMIT 25",
		"MATCH (:Objective)<-[:BUILDS_ON]-(h:Hypothesis {id: $id}) RETURN count(h)":                                                  "MATCH (:Objective {id: $pe})<-[:BUILDS_ON]-(h:Hypothesis {platform_experiment_id: $pe, id: $id}) RETURN count(h)",
		"MATCH (h:Hypothesis {platform_experiment_id: $pe})-[:BUILDS_ON]->(:Objective {id: $pe}) RETURN h":                           "MATCH (h:Hypothesis {platform_experiment_id: $pe})-[:BUILDS_ON]->(:Objective {id: $pe}) RETURN h",
		"MATCH (h:`Hypothesis` {}) RETURN (h.a + 1) * size('(n)') // (n)":                                                            "MATCH (h:`Hypothesis` {platform_experiment_id: $pe}) RETURN (h.a + 1) * size('(n)') // (n)",
		"MATCH (d:Hypothesis)-[:BUILDS_ON*1..3 {a: 1}]->(h:Hypothesis) RETURN d":                                                     "MATCH (d:Hypothesis {platform_experiment_id: $pe})-[:BUILDS_ON*1..3 {a: 1}" + hopScope + "]->(h:Hypothesis {platform_experiment_id: $pe}) RETURN d",
		"CALL text_search.search_all('i', 'x') YIELD score, node AS n WHERE score > 1 RETURN n":                                      "CALL text_search.search_all('i', 'x') YIELD score, node AS n WITH * WHERE n.platform_experiment_id = $pe WITH * WHERE score > 1 RETURN n",
		"MATCH (h:Hypothesis) WHERE exists((h:Hypothesis)-[:RELATED]->(:Hypothesis)) RETURN [(h:Hypothesis)-->(o:Objective) | o.id]": "MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE exists((h:Hypothesis {platform_experiment_id: $pe})-[:RELATED]->(:Hypothesis {platform_experiment_id: $pe})) RETURN [(h:Hypothesis {platform_experiment_id: $pe})-->(o:Objective {id: $pe}) | o.id]",
	} {
		if got, err := scoped(q); err != nil || got != want {
			t.Errorf("scoped(%q):\n got = (%q, %v)\nwant = %q", q, got, err, want)
		}
	}
}

// A query the rewrite cannot tie to $pe is refused before it is sent, whatever it would return.
func TestAQueryThatCannotBeScopedIsRefused(t *testing.T) {
	for _, q := range []string{
		"MATCH (h) RETURN h.id, h.text LIMIT 25",
		"MATCH (n) RETURN count(n)",
		"MATCH () RETURN count(*)",
		"MATCH ({id: $id}) RETURN 1",
		"MATCH (n {id: $id}) RETURN n.text",
		"MATCH (n $props) RETURN n.text",
		"MATCH (n:Hypothesis $props) RETURN n.text",
		"MATCH (n:Other) RETURN n.text",
		"MATCH (n:Hypothesis:Objective) RETURN n.text",
		"MATCH (n:Hypothesis|Other) RETURN n.text",
		"MATCH (n:$label) RETURN n.text",
		"MATCH (n:Hypothesis WHERE n.id = $id) RETURN n.text",
		"MATCH (n IS Hypothesis) RETURN n.text",
		"MATCH (n WHERE n.id = $id) RETURN n.text",
		"MATCH (h:Hypothesis {platform_experiment_id: $other}) RETURN h.text",
		"MATCH (h:Hypothesis {platform_experiment_id: 'pe-other'}) RETURN h.text",
		"MATCH (h:Hypothesis {platform_experiment_id: $pe + ''}) RETURN h.text",
		"MATCH (h:Hypothesis {id: $id, `platform_experiment_id`: $other}) RETURN h.text",
		"MATCH (o:Objective {id: 'pe-other'}) RETURN o",
		"MATCH (h:Hypothesis)-[:BUILDS_ON]->(p) RETURN p.text",
		"MATCH (h:Hypothesis)-[:RELATED]-() RETURN count(*)",
		"MATCH (h:Hypothesis) OPTIONAL MATCH (h)-[:RELATED]-(o) RETURN o.text",
		"MATCH (h:Hypothesis) RETURN [(h:Hypothesis)-[:RELATED]-(o) | o.text]",
		"MATCH (h:Hypothesis) WHERE exists((h:Hypothesis)-[:RELATED]-(o)) RETURN h.id",
		"MATCH (h:Hypothesis) WHERE EXISTS { MATCH (o) WHERE o.text CONTAINS 'x' } RETURN h.id",
		"MATCH (h:Hypothesis) CALL { MATCH (o) RETURN count(o) AS c } RETURN c",
		"MATCH (h:Hypothesis) RETURN h.id AS id UNION MATCH (o) RETURN o.id AS id",
		"MATCH (a:Hypothesis)-[* (e, n | true)]-(b:Hypothesis) RETURN b.id",
		"MATCH (a:Hypothesis)-[*BFS (e, n | true)]-(b:Hypothesis) RETURN b.id",
		"CALL text_search.search_all('i', 'x') YIELD node RETURN node.text UNION MATCH (n) RETURN n.text",
		"CALL text_search.search_all('i', 'x') YIELD score RETURN score",
		"CALL text_search.search_all('i', 'x') YIELD * RETURN node.text",
		"CALL text_search.search_all('i', 'x')",
		"MATCH (h:Hypothesis) RETURN mgps.await_validate(h)",
		"MATCH (h:Hypothesis) RETURN `nodes`(h)",
		"MATCH (h:Hypothesis) RETURN unknown_function(h.id)",
		"SHOW STORAGE INFO",
		"SHOW SCHEMA INFO",
		"EXPLAIN MATCH (h:Hypothesis) RETURN h",
		"// MATCH\rMATCH (n) RETURN n.text",
		"MATCH (h:Hypothesis RETURN h",
		"MATCH (n IS NOT Objective) RETURN n.text",
		"MATCH (h:Hypothesis) RETURN (h ; 1)",
		"MATCH (h:Hypothesis) RETURN (h h)",
		"MATCH (h:Hypothesis) RETURN (, h)",
		"MATCH (n$:Hypothesis) RETURN n$.text",
		"MATCH (h:Hypothesis) WITH h AS n$ RETURN n$.text",
		"MATCH (a:Hypothesis)-[*WSHORTEST 5]-(b:Hypothesis) RETURN b.id",
		"MATCH (a:Hypothesis)-[*ALLSHORTEST 5]-(b:Hypothesis) RETURN b.id",
		"MATCH (a:Hypothesis)-[*KSHORTEST|3]-(b:Hypothesis) RETURN b.id",
		"CALL text_search.search_all('i', 'x') YIELD node AS a, node AS b RETURN a.text",
		"WITH 1 AS x LOAD PARQUET FROM '/etc/passwd' AS row RETURN row",
		"WITH 1 AS x LOAD JSONL FROM '/etc/passwd' AS row RETURN row",
	} {
		if got, err := scoped(q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("scoped(%q): got = (%q, %v), want %v", q, got, err, ErrInvalidQuery)
		}
	}
}

// Memgraph reads far more runes as whitespace, name parts and dashes than this package's
// tokenizer could list: (n<U+001F>) and (n<U+20AC>) were sent as written and read every platform
// experiment. Outside a string literal and a backtick name every such rune is refused, wherever
// it stands.
func TestAQueryHoldsPrintableASCIIOutsideStringsAndBacktickNames(t *testing.T) {
	for _, r := range []rune{0x00, 0x08, 0x0b, 0x0c, 0x1c, 0x1f, 0x7f, 0x85, 0xa0, 0xad, 0xe9, 0x301, 0x1680, 0x2003, 0x2010, 0x200b, 0x2028, 0x20ac, 0x2212, 0x3000, 0x3008, 0xfeff} {
		for _, q := range []string{
			"MATCH (n%c) RETURN n",
			"MATCH (%cn) RETURN n",
			"MATCH (n%c:Hypothesis) RETURN n",
			"MATCH (a:Hypothesis)%c[*]%[1]c(b:Hypothesis) RETURN a",
			"MATCH (n:Hypothesis) RETURN n.id AS x%c",
			"MATCH (n:Hypothesis) RETURN n.id // %c",
			"MATCH (n:Hypothesis) RETURN n.id /* %c */",
		} {
			q = fmt.Sprintf(q, r)
			if got, err := scoped(q); !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("scoped(%+q): got = (%+q, %v), want %v", q, got, err, ErrInvalidQuery)
			}
		}
		for q, want := range map[string]string{
			"MATCH (n:Hypothesis {text: '%c'}) RETURN n":  "MATCH (n:Hypothesis {platform_experiment_id: $pe, text: '%c'}) RETURN n",
			"MATCH (n:Hypothesis) RETURN n.id AS `x%c`": "MATCH (n:Hypothesis {platform_experiment_id: $pe}) RETURN n.id AS `x%c`",
		} {
			q, want = fmt.Sprintf(q, r), fmt.Sprintf(want, r)
			if got, err := scoped(q); err != nil || got != want {
				t.Errorf("scoped(%+q):\n got = (%+q, %v)\nwant = %+q", q, got, err, want)
			}
		}
	}
}

// A parenthesis that is no node pattern is passed on only as an expression this package knows.
func TestAParenthesisedExpressionIsNotTakenForANodePattern(t *testing.T) {
	const match = "MATCH (h:Hypothesis {platform_experiment_id: $pe}) RETURN "
	for _, expression := range []string{
		"(h.a + 1)", "('a' + 'b')", "(\"a\")", "(-1)", "(+1)", "([1, 2])", "((h.a))", "(NOT true)", "(true AND false)",
		"(size([1]) > 0)", "(CASE WHEN true THEN 1 ELSE 2 END)", "(h.text STARTS WITH 'a')", "(1 IN [1])",
		"(h.a IS NULL)", "(h.a IS NOT NULL)", "(h.text =~ 'a')", "(h.a <> 1)", "(h.a % 2)", "(h.a ^ 2)", "(h.a / 2)", "(h.a * 2)",
		"(`h`.text CONTAINS 'a')", "(h.tags[0])", "(1.5)",
	} {
		if got, err := scoped(match + expression); err != nil || got != match+expression {
			t.Errorf("scoped(%q): got = (%q, %v), want it unchanged", match+expression, got, err)
		}
	}
}
