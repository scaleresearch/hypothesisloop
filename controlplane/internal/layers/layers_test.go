package layers

import (
	"os/exec"
	"strings"
	"testing"
)

const storagePath = "github.com/scaleresearch/hypothesisloop/controlplane/storage/"

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

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}
