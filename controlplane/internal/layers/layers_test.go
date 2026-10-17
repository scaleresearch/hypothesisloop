package layers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
)

const module = "github.com/scaleresearch/hypothesisloop/"

const storagePath = module + "controlplane/storage/"

// contractPath holds leaf packages shared with the CLI: any layer may import them, and they import nothing of the control plane.
const contractPath = "contract"

var datastores = []string{"db", "graphdb", "metricsdb", "objectstore"}

func TestDatastoresDoNotImportEachOther(t *testing.T) {
	for _, store := range datastores {
		t.Run(store, func(t *testing.T) {
			imported := goList(t, "-deps", "-f", "{{.ImportPath}}", storagePath+store)
			imported = append(imported, goList(t, "-f", `{{join .TestImports " "}} {{join .XTestImports " "}}`, storagePath+store)...)
			for _, path := range imported {
				for _, other := range datastores {
					if other != store && path == storagePath+other {
						t.Errorf("storage/%s imports storage/%s: a datastore is queried only through its own package", store, other)
					}
				}
			}
		})
	}
}

// layer is what the packages under one path of the control plane may import of this module, as
// paths relative to it: a path admits itself and everything below it, or only itself when exact.
// own admits the packages under the layer's own path, never refuses single packages the rest
// admits, and web admits huma and chi.
type layer struct {
	path  string
	may   []string
	never []string
	own   bool
	web   bool
	exact bool
}

var (
	base     = []string{"controlplane/domain", "controlplane/refusal"}
	services = append([]string{"controlplane/storage", "controlplane/clients", "controlplane/auth", "controlplane/config"}, base...)
)

// layers is matched by the longest path. A package no layer names is refused, so a new top-level
// package states where it stands.
var layers = []layer{
	// domain imports huma for a schema transform (cpu_quantity.go), the one accepted exception.
	{path: "controlplane/domain", own: true, web: true},
	{path: "controlplane/refusal"},
	{path: "controlplane/config", may: []string{"controlplane/domain"}},
	{path: "controlplane/auth", may: []string{"controlplane/config", "controlplane/domain", "controlplane/refusal"}},
	// authtest serves a test router.
	{path: "controlplane/auth/authtest", may: []string{"controlplane/auth", "controlplane/domain"}, web: true},
	{path: "controlplane/storage", may: []string{"controlplane/config", "controlplane/domain"}},
	{path: "controlplane/storage/objectstore", may: []string{"controlplane/config", "controlplane/domain"}, own: true},
	{path: "controlplane/clients", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}},
	{path: "controlplane/clients/githost", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}, own: true},
	// The GitHub client adapts itself to the App the accounts service declares (NewConnections),
	// so the service never imports a provider.
	{path: "controlplane/clients/githost/github", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal", "controlplane/clients/githost", "controlplane/service/accounts"}},
	{path: "controlplane/clients/registrycred", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}, own: true},
	// A service reaches another service only through an interface wired in cmd.
	{path: "controlplane/service", may: services},
	{path: "controlplane/service/scheduler", may: services, own: true},
	// Attach names the cluster-agent chart by the version its Chart.yaml gives.
	{path: "controlplane/service/accounts", may: append([]string{"deploy/charts/hypothesisloop-cluster-agent"}, services...)},
	{path: "controlplane/service/platformexperiments/platformexperimentstest", may: []string{"controlplane/domain", "controlplane/storage/db"}},
	{path: "controlplane/httpapi", may: append([]string{"controlplane/service", "controlplane/auth", "controlplane/config"}, base...), own: true, web: true},
	// The change stream reads the event store as it is written, the one HTTP package that does.
	{path: "controlplane/httpapi/watch", may: []string{"controlplane/storage/db", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}},
	// The git proxy forwards git's smart HTTP to the git host.
	{path: "controlplane/httpapi/gitproxy", may: []string{"controlplane/auth", "controlplane/clients/githost", "controlplane/domain", "controlplane/refusal"}, web: true},
	// The model proxy records each model call it forwards in the metrics store.
	{path: "controlplane/httpapi/modelproxy", may: []string{"controlplane/auth", "controlplane/domain", "controlplane/storage/metricsdb"}},
	// Metrics ingest writes what node-agents push into the metrics store.
	{path: "controlplane/httpapi/metricsingest", may: []string{"controlplane/auth", "controlplane/domain", "controlplane/refusal", "controlplane/storage/metricsdb"}},
	{path: "controlplane/httpapi/middleware", may: []string{"controlplane/auth"}},
	{path: "controlplane/cmd", may: []string{"controlplane"}, own: true, web: true},
	// What rents machines elsewhere reaches the control plane only through interfaces wired here.
	{path: "controlplane/cmd/control-service", may: []string{"controlplane", "runtime/cloud"}, web: true},
	{path: "controlplane/cmd/e2b-template", may: []string{"controlplane/config", "runtime/cloud/provider/e2b"}},
	// metrics-service serves node-agent pushes only, so it never mounts the API.
	{path: "controlplane/cmd/metrics-service", may: []string{"controlplane"}, never: []string{"controlplane/httpapi"}, web: true},
	{path: "controlplane/cmd/hl-admin", may: []string{"controlplane/auth", "controlplane/config", "controlplane/domain",
		"controlplane/clients/githost/github", "controlplane/storage/db"}, exact: true},
	{path: "controlplane/internal", may: []string{"controlplane"}, own: true, web: true},
	// The charts' tests load what the control-plane chart renders with the programs' own loader.
	{path: "deploy/charts/hypothesisloop", may: []string{"controlplane/config"}},
	{path: "deploy/charts/hypothesisloop-cluster-agent"},
}

func layerOf(path string) (layer, bool) {
	var found layer
	ok := false
	for _, l := range layers {
		if within(path, l.path) && len(l.path) > len(found.path) {
			found, ok = l, true
		}
	}
	return found, ok
}

func within(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func admits(l layer, imported string) bool {
	if within(imported, contractPath) {
		return true
	}
	if slices.Contains(l.never, imported) {
		return false
	}
	if l.own && within(imported, l.path) {
		return true
	}
	for _, may := range l.may {
		if l.exact && imported == may || !l.exact && within(imported, may) {
			return true
		}
	}
	return false
}

// Every package of the control plane imports only what its layer admits: handlers call services,
// services own the stores and clients, and the domain depends on nothing of the platform.
func TestEveryPackageImportsOnlyWhatItsLayerAdmits(t *testing.T) {
	lines := goListLines(t, "-f", `{{.ImportPath}} {{join .Imports " "}}`, module+"controlplane/...")
	// A chart's package is its tests alone.
	lines = append(lines, goListLines(t, "-f", `{{.ImportPath}} {{join .TestImports " "}}`, module+"deploy/...")...)
	for _, line := range lines {
		fields := strings.Fields(line)
		pkg := strings.TrimPrefix(fields[0], module)
		l, ok := layerOf(pkg)
		if !ok {
			t.Errorf("%s is in no layer", pkg)
			continue
		}
		for _, imported := range fields[1:] {
			if isWeb(imported) {
				if !l.web {
					t.Errorf("%s imports %s", pkg, imported)
				}
				continue
			}
			if !strings.HasPrefix(imported, module) {
				continue
			}
			imported = strings.TrimPrefix(imported, module)
			if !admits(l, imported) {
				t.Errorf("%s imports %s", pkg, imported)
			}
		}
	}
}

// A program reads the keys of the environment only through controlplane/config, so each reads its
// own and refuses to start without one.
func TestOnlyConfigReadsTheKeysOfTheEnvironment(t *testing.T) {
	for _, line := range goListLines(t, "-f", `{{.ImportPath}}|{{.Dir}}|{{join .GoFiles " "}}`, module+"...") {
		pkg, rest, _ := strings.Cut(strings.TrimPrefix(line, module), "|")
		dir, files, _ := strings.Cut(rest, "|")
		if pkg == "controlplane/config" {
			continue
		}
		for _, file := range strings.Fields(files) {
			parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, file), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "Getenv" && selector.Sel.Name != "LookupEnv") {
					return true
				}
				if receiver, ok := selector.X.(*ast.Ident); !ok || receiver.Name != "os" {
					return true
				}
				if literal, ok := call.Args[0].(*ast.BasicLit); ok {
					if name, err := strconv.Unquote(literal.Value); err == nil && config.IsKey(name) {
						t.Errorf("%s/%s reads %s, which only controlplane/config reads", pkg, file, name)
					}
				}
				return true
			})
		}
	}
}

func isWeb(path string) bool {
	return slices.ContainsFunc([]string{"github.com/danielgtaylor/huma/v2", "github.com/go-chi/chi/v5"}, func(web string) bool {
		return within(path, web)
	})
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}

func goListLines(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
