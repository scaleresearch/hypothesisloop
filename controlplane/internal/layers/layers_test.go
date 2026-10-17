package layers

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/scaleresearch/hypothesisloop/"

const storagePath = module + "controlplane/storage/"

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
	services = append([]string{"controlplane/storage", "controlplane/clients", "controlplane/auth", "controlplane/obsmetrics",
		"controlplane/config", "contract"}, base...)
)

// layers is matched by the longest path. A package no layer names is refused, so a new top-level
// package states where it stands.
var layers = []layer{
	// domain imports huma for a schema transform (cpu_quantity.go), the one accepted exception.
	{path: "controlplane/domain", may: []string{"contract/clusterapi"}, own: true, web: true},
	{path: "controlplane/refusal"},
	{path: "controlplane/config", may: []string{"controlplane/domain"}},
	{path: "controlplane/auth", may: []string{"controlplane/config", "controlplane/domain"}},
	// authtest serves a test router.
	{path: "controlplane/auth/authtest", may: []string{"controlplane/auth", "controlplane/domain"}, web: true},
	{path: "controlplane/obsmetrics", may: []string{"controlplane/domain"}},
	{path: "controlplane/storage", may: []string{"controlplane/config", "controlplane/domain"}},
	{path: "controlplane/storage/objectstore", may: []string{"controlplane/config", "controlplane/domain"}, own: true},
	{path: "controlplane/clients", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}},
	{path: "controlplane/clients/githost", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}, own: true},
	{path: "controlplane/clients/registrycred", may: []string{"controlplane/config", "controlplane/auth", "controlplane/domain", "controlplane/refusal"}, own: true},
	// A service reaches another service only through an interface wired in cmd.
	{path: "controlplane/service", may: services},
	{path: "controlplane/service/scheduler", may: services, own: true},
	{path: "controlplane/service/platformexperiments/platformexperimentstest", may: []string{"controlplane/domain", "controlplane/storage/db"}},
	{path: "controlplane/httpapi", may: append([]string{"controlplane/service", "controlplane/auth", "controlplane/config", "contract"}, base...), own: true, web: true},
	// The change stream reads the event store as it is written, the one HTTP package that does.
	{path: "controlplane/httpapi/watch", may: []string{"controlplane/storage/db", "controlplane/auth", "controlplane/domain"}},
	// The git proxy forwards git's smart HTTP to the git host.
	{path: "controlplane/httpapi/gitproxy", may: []string{"controlplane/auth", "controlplane/clients/githost", "controlplane/domain"}, web: true},
	// The model proxy records each model call it forwards in the metrics store.
	{path: "controlplane/httpapi/modelproxy", may: []string{"controlplane/auth", "controlplane/domain", "controlplane/storage/metricsdb"}},
	// Metrics ingest writes what node-agents push into the metrics store.
	{path: "controlplane/httpapi/metricsingest", may: []string{"contract/clusterapi", "controlplane/auth", "controlplane/domain", "controlplane/storage/metricsdb"}},
	{path: "controlplane/httpapi/middleware", may: []string{"controlplane/auth"}},
	// What rents machines elsewhere reaches the control plane only through interfaces wired in cmd.
	{path: "controlplane/cloud", may: []string{"controlplane/cloud/provider", "contract", "runtime/shared"}, own: true},
	{path: "controlplane/cmd", may: []string{"controlplane", "contract", "runtime/shared"}, own: true, web: true},
	// metrics-service serves node-agent pushes only, so it never mounts the API.
	{path: "controlplane/cmd/metrics-service", may: []string{"controlplane", "contract", "runtime/shared"}, never: []string{"controlplane/httpapi"}, web: true},
	{path: "controlplane/cmd/hl-admin", may: []string{"controlplane/auth", "controlplane/config", "controlplane/domain",
		"controlplane/clients/githost/github", "controlplane/storage/db"}, exact: true},
	{path: "controlplane/internal", may: []string{"controlplane"}, own: true, web: true},
	{path: "controlplane/infra"},
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
	for _, line := range goListLines(t, "-f", `{{.ImportPath}} {{join .Imports " "}}`, module+"controlplane/...") {
		fields := strings.Fields(line)
		pkg := strings.TrimPrefix(fields[0], module)
		if strings.HasPrefix(pkg, "controlplane/ui/") {
			continue
		}
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
