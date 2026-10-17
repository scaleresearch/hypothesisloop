//go:build !e2efake

package providers

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestABinaryBuiltWithoutTheTestTagCannotBeConfiguredWithTheInMemoryProvider(t *testing.T) {
	if _, err := New("fake", yaml.Node{}, "", nil, t.Logf); err == nil || !strings.Contains(err.Error(), `"fake"`) {
		t.Fatalf("err = %v, want the name refused", err)
	}
}
