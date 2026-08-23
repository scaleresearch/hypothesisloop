package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// evictionReasonsDeclaredInSource reads the reason vocabulary out of the package source rather
// than a list maintained alongside it. A hand-kept list is exactly the thing that goes stale:
// whoever adds a sixteenth reason has no reason to think of it, so the completeness check it
// backs would keep passing while the taxonomy silently developed a hole.
//
// It walks every .go file in the package, not just constants.go, and it understands all three
// ways Go lets a constant carry this type — an explicit type on the spec, a type inherited from
// an earlier spec in the same const block, and a conversion expression. An earlier version
// matched only "explicitly typed string literal in constants.go", which meant a reason declared
// any other way was invisible to the very check that exists to make declaring one impossible
// without classifying it.
func evictionReasonsDeclaredInSource(t *testing.T) map[EvictionReason]string {
	t.Helper()

	// Derived from this file's own path rather than a relative "constants.go": the parser
	// resolves relative paths against the process working directory, which `go test` happens to
	// set to the package directory but a directly-invoked test binary does not.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file to find the package source")
	}
	dir := filepath.Dir(thisFile)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse package %s: %v", dir, err)
	}

	// name -> value, so a duplicate value is reported as the two identifiers that collide rather
	// than silently collapsing into one map key.
	reasons := map[EvictionReason]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				collectEvictionReasons(t, gen, reasons)
			}
		}
	}

	if len(reasons) == 0 {
		t.Fatal("found no EvictionReason constants in the package — the parser stopped matching the source")
	}
	return reasons
}

func isEvictionReasonIdent(e ast.Expr) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == "EvictionReason"
}

// collectEvictionReasons applies Go's own const-block rules: a spec with no type and no values
// repeats the previous spec's type and values, which is how a reason could otherwise be declared
// entirely invisibly.
func collectEvictionReasons(t *testing.T, gen *ast.GenDecl, out map[EvictionReason]string) {
	t.Helper()
	var inheritedType ast.Expr
	var inheritedValues []ast.Expr

	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		specType, values := vs.Type, vs.Values
		if specType == nil && len(values) == 0 {
			specType, values = inheritedType, inheritedValues
		} else {
			inheritedType, inheritedValues = vs.Type, vs.Values
		}

		for i, name := range vs.Names {
			if i >= len(values) {
				continue
			}
			value := values[i]

			isReason := isEvictionReasonIdent(specType)
			if !isReason {
				// A conversion: `const Foo = EvictionReason("foo")`, which carries the type
				// without ever naming it on the spec.
				if call, isCall := value.(*ast.CallExpr); isCall && isEvictionReasonIdent(call.Fun) && len(call.Args) == 1 {
					isReason = true
					value = call.Args[0]
				}
			}
			if !isReason {
				continue
			}

			lit, ok := value.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("EvictionReason constant %s has a non-literal value the vocabulary check cannot read", name.Name)
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}
			if existing, dup := out[EvictionReason(unquoted)]; dup {
				t.Errorf("eviction reasons %s and %s share the wire value %q — one of them can never be classified distinctly", existing, name.Name, unquoted)
			}
			out[EvictionReason(unquoted)] = name.Name
		}
	}
}

// The whole point of the taxonomy: every terminal outcome is classified from the typed reason it
// already carries, exhaustively. A reason with no class is a test failure, not a default, because
// a default would silently absorb every future reason into whichever class was convenient — and
// since infrastructure is the class that refunds quota and forgives a failure, an unclassified
// reason landing there by accident is a real correctness bug, not a cosmetic one.
func TestEveryEvictionReasonHasExactlyOneFaultClass(t *testing.T) {
	declared := evictionReasonsDeclaredInSource(t)

	for reason := range declared {
		class, found := Classify(reason)
		if !found {
			t.Errorf("eviction reason %q has no fault class — decide whether it is workload, infrastructure or policy and add it to faultClasses", reason)
			continue
		}
		switch class {
		case FaultWorkload, FaultInfrastructure, FaultPolicy:
		default:
			t.Errorf("eviction reason %q has class %q, which is not one of the three", reason, class)
		}
	}

	// The other direction: a class entry for a reason that no longer exists is dead weight that
	// makes the table read as more complete than it is.
	for reason := range faultClasses {
		if _, inSource := declared[reason]; !inSource {
			t.Errorf("faultClasses classifies %q, which is not a declared EvictionReason", reason)
		}
	}
	if len(faultClasses) != len(declared) {
		t.Errorf("faultClasses has %d entries for %d declared reasons", len(faultClasses), len(declared))
	}
}

// Classification is driven by the typed code and never by message text. WithDetail is appended to
// most real evictions, so a Classify that did not strip it would find nothing for nearly every
// reason it was actually asked about — and "not found" is not a class, so the consequences that
// hang off infrastructure would silently stop applying in production while still passing every
// test written against bare constants.
func TestClassifyStripsTheWithDetailSuffix(t *testing.T) {
	detailed := EvictionClusterUnreachable.WithDetail("no snapshot for 412s")
	if !strings.Contains(string(detailed), ":") {
		t.Fatalf("WithDetail produced %q, expected a detail suffix to strip", detailed)
	}

	class, found := Classify(detailed)
	if !found {
		t.Fatalf("Classify(%q) found nothing — the detail suffix was not stripped", detailed)
	}
	if class != FaultInfrastructure {
		t.Errorf("Classify(%q) = %q, want %q", detailed, class, FaultInfrastructure)
	}
	if !IsInfrastructureFault(detailed) {
		t.Error("IsInfrastructureFault said no for a detailed cluster_unreachable — its refund and free requeue would never fire in production")
	}
}

// An unrecognised reason must report itself as unrecognised rather than being handed a class.
// This is what a reason written by an older or newer build of the control plane looks like, and
// answering "infrastructure" for it would refund quota on a guess.
func TestClassifyReportsAnUnknownReasonRatherThanDefaultingIt(t *testing.T) {
	if class, found := Classify(EvictionReason("invented_reason")); found {
		t.Errorf("Classify returned class %q for an unknown reason, want found=false", class)
	}
	if IsInfrastructureFault(EvictionReason("invented_reason")) {
		t.Error("an unknown reason was treated as an infrastructure fault — that refunds quota and forgives a failure on a guess")
	}
}

// The three classes must actually partition the vocabulary: a taxonomy where every reason landed
// in one bucket would pass the exhaustiveness test above while telling nobody anything.
func TestEachFaultClassIsUsed(t *testing.T) {
	seen := map[FaultClass]int{}
	for _, class := range faultClasses {
		seen[class]++
	}
	for _, class := range []FaultClass{FaultWorkload, FaultInfrastructure, FaultPolicy} {
		if seen[class] == 0 {
			t.Errorf("no eviction reason is classified %q", class)
		}
	}
}

// Code() splits on the first colon, so a reason whose own wire value contained one would be
// truncated to the part before it and classified as something else entirely -- or as nothing.
// Nothing enforces that today except this, and the failure would be silent: WithDetail is
// appended in production but almost never in a unit test, so a colon-bearing reason would pass
// every test written against the bare constant and misclassify every real eviction.
func TestNoEvictionReasonContainsAColon(t *testing.T) {
	for reason, name := range evictionReasonsDeclaredInSource(t) {
		if strings.Contains(string(reason), ":") {
			t.Errorf("eviction reason %s = %q contains a colon, which Code() treats as the start of a WithDetail suffix", name, reason)
		}
	}
}

// Every declared reason must survive the round trip Code() performs on it, detail or not. This is
// the property Classify actually depends on, asserted over the whole vocabulary rather than the
// one reason TestClassifyStripsTheWithDetailSuffix happens to name.
func TestEveryReasonClassifiesTheSameWithAndWithoutDetail(t *testing.T) {
	for reason, name := range evictionReasonsDeclaredInSource(t) {
		bare, bareFound := Classify(reason)
		detailed, detailedFound := Classify(reason.WithDetail("some job-specific detail"))
		if !bareFound || !detailedFound || bare != detailed {
			t.Errorf("%s: bare = (%q, %v), detailed = (%q, %v) -- a reason must classify identically either way",
				name, bare, bareFound, detailed, detailedFound)
		}
	}
}

// The breakdown has to account for every failure it was given. A class list that silently drops
// what it cannot classify reads as "no such failures" — the single most misleading thing this
// endpoint could tell an agent deciding whether to keep debugging its own code.
func TestClassifyCountsAccountsForEveryReasonIncludingUnknownOnes(t *testing.T) {
	byReason := map[string]int{
		string(EvictionSilent):               2, // workload
		string(EvictionNeverReportedMetrics): 1, // workload
		string(EvictionClusterUnreachable):   3, // infrastructure
		string(EvictionStageCut):             4, // policy
		"a_reason_from_another_build":        5, // unclassified
	}

	byClass := ClassifyCounts(byReason)
	want := map[FaultClass]int{
		FaultWorkload:       3,
		FaultInfrastructure: 3,
		FaultPolicy:         4,
		FaultUnclassified:   5,
	}
	for class, wantCount := range want {
		if got := byClass[class]; got != wantCount {
			t.Errorf("ClassifyCounts()[%q] = %d, want %d", class, got, wantCount)
		}
	}

	total, classified := 0, 0
	for _, c := range byReason {
		total += c
	}
	for _, c := range byClass {
		classified += c
	}
	if total != classified {
		t.Errorf("classes sum to %d but %d failures were counted — the breakdown loses failures", classified, total)
	}
}

// Reasons carry a per-job detail suffix in production (WithDetail), and the stats path keys its
// tally by Code(). Asserting the folded form handles a detailed key anyway costs nothing and
// removes the assumption that every caller remembered to strip it first.
func TestClassifyCountsFoldsDetailedReasonsIntoTheirClass(t *testing.T) {
	byClass := ClassifyCounts(map[string]int{
		string(EvictionClusterUnreachable.WithDetail("no snapshot for 412s")): 1,
		string(EvictionClusterUnreachable):                                    1,
	})
	if got := byClass[FaultInfrastructure]; got != 2 {
		t.Errorf("infrastructure count = %d, want 2 — a detailed reason was not folded with its bare form", got)
	}
	if got := byClass[FaultUnclassified]; got != 0 {
		t.Errorf("%d failure(s) landed in unclassified — the detail suffix was not stripped", got)
	}
}

// The checkpoint window is granted to exactly one class. A job the platform itself decided to
// stop was doing nothing wrong and its work is worth saving; a job the environment killed has
// nothing left to save it with, and a job its own code killed has nothing to save. Granting the
// window on class rather than on status is what keeps a preempted job (requeued, still holding
// its accelerators for a moment) apart from a job whose node vanished.
func TestOnlyAPolicyTerminationGrantsACheckpointWindow(t *testing.T) {
	exps := []*Experiment{
		{ID: "preempted", EvictionReason: string(EvictionPreemptedForGuaranteed)},
		{ID: "node-gone", EvictionReason: string(EvictionWorkloadGone)},
		{ID: "bad-image", EvictionReason: string(EvictionUnschedulable)},
		{ID: "stage-cut", EvictionReason: string(EvictionStageCut)},
	}
	granted := CheckpointWindowGrants(exps)
	if len(granted) != 2 || granted[0] != "preempted" || granted[1] != "stage-cut" {
		t.Fatalf("granted = %v, want [preempted stage-cut]: only the platform's own decisions earn a window", granted)
	}
}

// An experiment that ended with no eviction reason at all -- it simply completed -- is not a
// termination and must not be handed a window. It reaches this list only because it stopped
// being desired, which every finished job does.
func TestAnExperimentWithNoEvictionReasonGrantsNoCheckpointWindow(t *testing.T) {
	granted := CheckpointWindowGrants([]*Experiment{{ID: "completed"}})
	if len(granted) != 0 {
		t.Fatalf("granted = %v, want none: a job that finished on its own was never terminated", granted)
	}
}
