package graphdb

import (
	"errors"
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
	} {
		if got, err := scoped(q); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("scoped(%q): got = (%q, %v), want %v", q, got, err, ErrInvalidQuery)
		}
	}
}
