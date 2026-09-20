package e2b

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// The names below are what infra/images/e2b-template.sh printed when it built these templates.
func TestTemplateNameIsTheOneTheBuildScriptGives(t *testing.T) {
	cases := map[string]struct {
		image string
		size  size
	}{
		"hl-2adc8dc5669db1ed0a9b-1c-1024m": {"alpine/curl:8.14.1", size{CPUs: 1, MemoryMB: 1024}},
		"hl-cf1b6e9cc15cc669d1c0-2c-4096m": {"python:3.13-slim", size{CPUs: 2, MemoryMB: 4096}},
	}
	for want, tc := range cases {
		if got := templateName(tc.image, tc.size); got != want {
			t.Errorf("templateName(%q, %+v) = %q, want %q", tc.image, tc.size, got, want)
		}
	}
}

func TestTemplateNameTellsImagesAndSizesApartInWhatAVendorNameMayHold(t *testing.T) {
	allowed := regexp.MustCompile(`^[a-z0-9-]+$`)
	seen := map[string]bool{}
	for _, image := range []string{"registry.example/agent:1", "registry.example/agent:2", "Registry.Example:5000/Team/Agent@sha256:" + strings.Repeat("a", 64)} {
		for _, s := range []size{{1, 1024}, {2, 1024}, {2, 4096}} {
			name := templateName(image, s)
			if !allowed.MatchString(name) || len(name) > 128 {
				t.Errorf("templateName(%q, %+v) = %q, which a template cannot be named", image, s, name)
			}
			if seen[name] {
				t.Errorf("templateName(%q, %+v) = %q, which another image or size has", image, s, name)
			}
			seen[name] = true
		}
	}
}

func TestSizeRoundsUpToWholeCoresAndMebibytes(t *testing.T) {
	cases := map[size]provider.Spec{
		{CPUs: 2, MemoryMB: 4096}: {CPUMillis: 2000, MemoryBytes: 4 << 30},
		{CPUs: 1, MemoryMB: 1}:    {CPUMillis: 1, MemoryBytes: 1},
		{CPUs: 3, MemoryMB: 1025}: {CPUMillis: 2001, MemoryBytes: 1<<30 + 1},
	}
	for want, spec := range cases {
		if got := sizeOf(spec); got != want {
			t.Errorf("sizeOf(%d millicores, %d bytes) = %+v, want %+v", spec.CPUMillis, spec.MemoryBytes, got, want)
		}
	}
}

func TestTheBuildScriptThisPackageNamesExistsAndWritesTheFileTheSupervisorReads(t *testing.T) {
	script, err := os.ReadFile("../../../../../" + templateScript)
	if err != nil {
		t.Fatalf("the script named in Create's error: %v", err)
	}
	if !strings.Contains(string(script), "> "+imageEnvPath) {
		t.Errorf("%s does not write %s, which every launch file reads first", templateScript, imageEnvPath)
	}
}
