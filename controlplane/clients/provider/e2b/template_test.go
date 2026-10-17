package e2b

import (
	"regexp"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// A template is named by its image's digest and its size, so a name a template was built under
// stays the one Create asks for.
func TestTemplateNameIsTheOneBuiltBefore(t *testing.T) {
	if got := templateName("sha256:"+strings.Repeat("a", 64), size{CPUs: 2, MemoryMB: 4096}); got != agentTemplate {
		t.Errorf("templateName = %q, want %q", got, agentTemplate)
	}
}

func TestTemplateNameTellsImagesAndSizesApartInWhatAVendorNameMayHold(t *testing.T) {
	allowed := regexp.MustCompile(`^[a-z0-9-]+$`)
	seen := map[string]bool{}
	for _, image := range []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64)} {
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
