package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// A planner whose file had a colon in its first line was told no line, and spent six commands
// installing a parser to find it. Every file hl reads is refused with where and what it reads there.
func TestAFileThatIsNotYAMLIsRefusedWithItsLineColumnAndText(t *testing.T) {
	file := filepath.Join(t.TempDir(), "proposal.yaml")
	clitest.WriteJob(t, file, "experiment:\n  name: SHA-256 throughput: on CPU\n")
	t.Setenv("HL_TOKEN", "hlk_x")
	t.Setenv("API_URL", "http://127.0.0.1:1")
	want := file + " is not valid YAML: line 2, column 27: mapping values are not allowed in this context, where the line reads: name: SHA-256 throughput: on CPU"
	for _, args := range [][]string{
		{"platform-experiments", "propose", "--platform-experiment", "pe-1", file},
		{"job", "submit", "--agent", "ag-1", file},
		{"hypothesis", "submit", "--agent", "ag-1", "--file", file},
	} {
		if code, _, stderr := hl(t, args...); code != 2 || strings.TrimSpace(stderr) != "hl: "+want {
			t.Errorf("hl %s = %d\n%s\nwant only\nhl: %s", strings.Join(args[:2], " "), code, stderr, want)
		}
	}
}
