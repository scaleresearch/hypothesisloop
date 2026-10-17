package platformexperiments

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Every reason a baseline is refused for becomes the experiment's status_reason, and reading an
// experiment whose reason names nobody it waits for fails.
func TestEveryBaselineRefusalNamesWhoActsOnIt(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	refusal := regexp.MustCompile(`refusedBaseline\("([a-z_]+)"`)
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range refusal.FindAllStringSubmatch(string(source), -1) {
			if _, err := domain.WaitsFor(m[1], true); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
	}
}
