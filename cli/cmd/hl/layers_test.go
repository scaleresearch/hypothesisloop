package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const internal = "github.com/scaleresearch/hypothesisloop/cli/internal/"

// imports is each package of the CLI with the packages it imports, all of them with deps.
func imports(t *testing.T, deps bool) map[string][]string {
	t.Helper()
	field := ".Imports"
	if deps {
		field = ".Deps"
	}
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}}{{range "+field+"}} {{.}}{{end}}", "github.com/scaleresearch/hypothesisloop/cli/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	listed := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		listed[fields[0]] = fields[1:]
	}
	return listed
}

// The commands of each audience change without the others: none of their packages imports
// another's, the shared helpers sit below them all, and the test helpers in no command.
func TestThePackagesOfTheCLIImportOnlyTheLayersBelowThem(t *testing.T) {
	deps, direct := imports(t, true), imports(t, false)
	audiences := []string{"people", "agent", "shared", "jobexec"}
	for _, a := range audiences {
		for _, b := range audiences {
			if a != b && slices.Contains(deps[internal+a], internal+b) {
				t.Errorf("%s imports %s", a, b)
			}
		}
	}
	for pkg, imported := range deps {
		if pkg != internal+"clitest" && slices.Contains(imported, internal+"clitest") {
			t.Errorf("%s imports clitest, which only tests may", pkg)
		}
	}
	for _, path := range direct[internal+"command"] {
		if strings.HasPrefix(path, internal) && path != internal+"apiclient" {
			t.Errorf("command imports %s: it imports apiclient alone of the CLI", path)
		}
	}
	for _, path := range direct[internal+"apiclient"] {
		if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") && !strings.HasPrefix(path, "github.com/scaleresearch/hypothesisloop/contract/") {
			t.Errorf("apiclient imports %s: it imports the contract and the standard library alone", path)
		}
	}
}
