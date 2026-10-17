package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// announcedInserts are the tables a row inserted into is a change GET /watch delivers.
var announcedInserts = map[string]bool{
	"experiments": true, "agent_quotas": true, "hypothesis_findings": true, "hypothesis_comments": true,
	"platform_experiment_stage_advances": true, "platform_experiment_cuts": true,
	"baseline_raises": true, "agent_messages": true, "platform_rules": true,
	"platform_experiments": true, "experiment_signups": true, "hosted_instances": true,
}

// announcedUpdates are the columns, per table, a write of which is a change GET /watch delivers. An
// empty set is every column.
var announcedUpdates = map[string][]string{
	"experiments":          {"status", "not_admitted_reason", "underperforming_metric", "held_reason", "same_hypothesis"},
	"platform_experiments": {"status", "description", "duration_hours", "coordinator_reason", "researchers_held", "capacity_noticed_at", "goal", "proposed_at"},
	"agent_quotas":         {},
	"experiment_signups":   {"finished_at", "idle_noticed_at"},
	"environment_waiters":  {"loaded"},
	"agent_types":          {"current_version"},
	"hosted_instances":     {"state", "activity"},
}

// appendOnly are the tables no write updates or deletes.
var appendOnly = map[string]bool{"platform_rules": true, "agent_type_events": true, "agent_type_versions": true}

var (
	insertInto = regexp.MustCompile(`(?is)\bINSERT\s+INTO\s+(\w+)`)
	updateSet  = regexp.MustCompile(`(?is)\bUPDATE\s+(\w+)(?:\s+(?:AS\s+)?\w+)?\s+SET\b`)
	deleteFrom = regexp.MustCompile(`(?is)\bDELETE\s+FROM\s+(\w+)`)
)

// storeFunc is one function of the store: the SQL it holds, whether it announces a change itself,
// and the functions of this package it calls or hands on.
type storeFunc struct {
	sql       []string
	announces bool
	uses      []string
}

// Every write of the store that changes what GET /watch delivers announces it in its own
// transaction: a function holding such a write calls Change, or calls or hands on a function of
// this package that does. And nothing updates or deletes a row of an append-only table.
func TestEveryAnnouncedWriteAnnouncesAndAppendOnlyTablesAreOnlyAppended(t *testing.T) {
	funcs := storeFuncs(t)
	announces := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, f := range funcs {
			if !announces[name] && (f.announces || slices.ContainsFunc(f.uses, func(u string) bool { return announces[u] })) {
				announces[name], changed = true, true
			}
		}
	}
	for name, f := range funcs {
		for _, sql := range f.sql {
			for _, write := range writesIn(sql) {
				if write.appendOnly {
					t.Errorf("%s: %s, and the table is append-only", name, write.what)
				}
				if write.announced && !announces[name] {
					t.Errorf("%s: %s and announces no change", name, write.what)
				}
			}
		}
	}
}

type sqlWrite struct {
	what                  string
	announced, appendOnly bool
}

func writesIn(sql string) []sqlWrite {
	var writes []sqlWrite
	for _, m := range insertInto.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		writes = append(writes, sqlWrite{what: "inserts into " + table, announced: announcedInserts[table]})
	}
	for _, m := range updateSet.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		if table == "set" {
			continue
		}
		columns, watched := announcedUpdates[table]
		set := setClause(sql[m[1]:])
		announced := watched && (len(columns) == 0 || slices.ContainsFunc(columns, func(c string) bool {
			return regexp.MustCompile(`(?i)(^|[\s,])` + c + `\s*=`).MatchString(set)
		}))
		writes = append(writes, sqlWrite{what: "updates " + table, announced: announced, appendOnly: appendOnly[table]})
	}
	for _, m := range deleteFrom.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		writes = append(writes, sqlWrite{what: "deletes from " + table, announced: table == "environment_waiters" || table == "experiment_signups", appendOnly: appendOnly[table]})
	}
	return writes
}

// setClause is the SET list at the start of rest, up to the WHERE, FROM or RETURNING outside
// parentheses that ends it.
func setClause(rest string) string {
	depth := 0
	end := regexp.MustCompile(`(?i)^\s(WHERE|FROM|RETURNING)\b`)
	for i, r := range rest {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case depth == 0 && end.MatchString(rest[i:]):
			return rest[:i]
		}
	}
	return rest
}

// storeFuncs parses the store's own files into their functions, by name: Type.Method for a
// method, the name alone for a function.
func storeFuncs(t *testing.T) map[string]*storeFunc {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	consts := map[string]ast.Expr{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, spec := range g.Specs {
					v := spec.(*ast.ValueSpec)
					for i, n := range v.Names {
						if i < len(v.Values) {
							consts[n.Name] = v.Values[i]
						}
					}
				}
			}
		}
	}
	methods := map[string][]string{}
	funcs := map[string]*storeFunc{}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := funcName(fn)
			methods[fn.Name.Name] = append(methods[fn.Name.Name], name)
			sf := &storeFunc{}
			funcs[name] = sf
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BinaryExpr, *ast.BasicLit:
					if sql := stringOf(n.(ast.Expr), consts); sql != "" {
						sf.sql = append(sf.sql, sql)
					}
					return false
				case *ast.Ident:
					if _, ok := consts[n.Name]; ok {
						sf.sql = append(sf.sql, stringOf(n, consts))
					}
					sf.uses = append(sf.uses, n.Name)
				case *ast.SelectorExpr:
					if n.Sel.Name == "Change" || n.Sel.Name == "ChangeUnlessAnnounced" {
						sf.announces = true
					}
					sf.uses = append(sf.uses, n.Sel.Name)
				}
				return true
			})
		}
	}
	for _, sf := range funcs {
		var uses []string
		for _, u := range sf.uses {
			uses = append(uses, methods[u]...)
		}
		sf.uses = uses
	}
	return funcs
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// stringOf is the text of a string expression: its literals and the package's constants it names
// joined, anything else a space.
func stringOf(e ast.Expr, consts map[string]ast.Expr) string {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return " "
		}
		return stringOf(e.X, consts) + stringOf(e.Y, consts)
	case *ast.Ident:
		if v, ok := consts[e.Name]; ok {
			return stringOf(v, consts)
		}
		return " "
	case *ast.ParenExpr:
		return stringOf(e.X, consts)
	}
	return " "
}
