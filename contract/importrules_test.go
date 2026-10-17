package contract

import (
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/scaleresearch/hypothesisloop/"

// testOnlyExceptions are the test files of the CLI that import the control plane, each because it
// runs the control plane's own code against the other side.
var testOnlyExceptions = map[string]string{
	"cli/cmd/hl/platformgate_test.go":       "runs the CLI against the real control plane services",
	"cli/internal/apiclient/caller_test.go": "reads tokens the control plane mints",
}

// The CLI shares only the contract with the control plane, so either side can change everything
// else without the other.
func TestTheCLIDoesNotImportTheControlPlane(t *testing.T) {
	for _, path := range goList(t, "-deps", "-f", "{{.ImportPath}}", module+"cli/...") {
		if strings.HasPrefix(path, module+"controlplane/") {
			t.Errorf("the CLI imports %s", path)
		}
	}
	root := repoRoot(t)
	for _, dir := range goList(t, "-f", "{{.Dir}}", module+"cli/...") {
		files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			rel, err := filepath.Rel(root, file)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range fileImports(t, file) {
				if strings.HasPrefix(path, module+"controlplane/") && testOnlyExceptions[filepath.ToSlash(rel)] == "" {
					t.Errorf("%s imports %s", rel, path)
				}
			}
		}
	}
}

// The runtime is the software deployed onto machines, so it imports only the contract and its own
// packages: never the control plane, and never the CLI. The control plane reaches into runtime/
// only where controlplane/internal/layers admits it.
func TestTheRuntimeImportsOnlyTheContractAndItself(t *testing.T) {
	lines := goListLines(t, "-f", `{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`, module+"runtime/...")
	for _, line := range lines {
		fields := strings.Fields(line)
		for _, path := range fields[1:] {
			if strings.HasPrefix(path, module) && !strings.HasPrefix(path, module+"runtime/") && !strings.HasPrefix(path, module+"contract/") {
				t.Errorf("%s imports %s", strings.TrimPrefix(fields[0], module), path)
			}
		}
	}
}

// The contract is what both sides import, so it imports neither.
func TestTheContractImportsNothingOfThePlatform(t *testing.T) {
	packages := goList(t, "-f", "{{.ImportPath}}", module+"contract/...")
	imported := goList(t, "-deps", "-test", "-f", "{{.ImportPath}}", module+"contract/...")
	for _, path := range imported {
		path, _, _ = strings.Cut(path, " ")
		path = strings.TrimSuffix(path, ".test")
		if strings.HasPrefix(path, module) && !strings.HasPrefix(path, module+"contract/") && !contains(packages, path) {
			t.Errorf("the contract imports %s", path)
		}
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func fileImports(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(parsed.Imports))
	for _, spec := range parsed.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dirs := goList(t, "-m", "-f", "{{.Dir}}")
	if len(dirs) != 1 {
		t.Fatalf("go list -m = %v, want the one module", dirs)
	}
	return dirs[0]
}

func goListLines(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}
