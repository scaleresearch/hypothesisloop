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
		"MATCH (h:Hypothesis) RETURN h.id":                                                 false,
		"CALL text_search.search('hypothesis_text', 'data.text:x') YIELD node RETURN node": false,
		"CALL `text_search`.`search_all`('hypothesis_text', 'x') YIELD node RETURN node":   false,
		"MATCH (n) CALL { WITH n RETURN n.id AS id } RETURN id":                            false,
		"RETURN 'LOAD CSV, USING PERIODIC COMMIT, CALL mg.load()' AS s":                    false,
		"MATCH (n) WHERE n.call = 1 OR n.load = 2 RETURN n /* CALL mg.load_all() */":       false,
		"call mg.load_all()":                              true,
		"CALL `mg.create_module_file`('a.py', '')":        true,
		"CALL mg//x\n.create_module_file('a.py', '')":     true,
		"CALL text_search.search_edges('i', 'x')":         true,
		"LoAd CsV FROM '/etc/passwd' AS r RETURN r":       true,
		"USING periodic COMMIT 1 MATCH (n) RETURN n":      true,
		"CALL { CREATE (:X) } IN TRANSACTIONS OF 1 ROWS":  true,
		"CALL { CREATE (:X) } IN CONCURRENT TRANSACTIONS": true,
		"RETURN 'unterminated":                            true,
		"MATCH (n) RETURN n /* unterminated":              true,
	} {
		err := checkReadOnly(q)
		if got := errors.Is(err, ErrInvalidQuery); got != refused {
			t.Errorf("checkReadOnly(%q): got = %v, want refused = %v", q, err, refused)
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
