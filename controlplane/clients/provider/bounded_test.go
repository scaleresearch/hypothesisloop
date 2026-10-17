package provider_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/fake"
)

func TestAVendorThatEchoesTheRequestNeverPutsASecretInAnError(t *testing.T) {
	const token, password = "agent-token-0123456789", "registry-password-9876"
	spec := provider.Spec{
		Name: "agent-a", Image: "registry.example/agent:1", Architecture: "amd64", Command: []string{"run"},
		CPUMillis: 1000, MemoryBytes: 1 << 30, DiskBytes: 1 << 30,
		Env:        map[string]string{"HYPOTHESISLOOP_TOKEN": token, "ACCELERATOR_TYPE": "small"},
		ImageLogin: &provider.ImageLogin{Registry: "registry.example", Username: "robot", Password: password},
	}
	vendor := fake.New()
	vendor.Fail = func(method, name string) error {
		echoed := fmt.Sprintf(`HTTP 400: {"env":{"HYPOTHESISLOOP_TOKEN":%q,"ACCELERATOR_TYPE":"small"},"image_login":"-u robot -p %s registry.example"}`, token, password)
		return fmt.Errorf("%s: %w", echoed, provider.ErrNoOffer)
	}
	bounded := provider.Bounded(vendor)

	_, createErr := bounded.Create(context.Background(), spec)
	warmErr := bounded.(provider.Warmer).StartWarm(context.Background(), "some-id", spec)
	for call, err := range map[string]error{"Create": createErr, "StartWarm": warmErr} {
		if err == nil {
			t.Fatalf("%s: the vendor's refusal was lost", call)
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), password) {
			t.Fatalf("%s: the error quotes a secret: %v", call, err)
		}
		if !strings.Contains(err.Error(), "<HYPOTHESISLOOP_TOKEN>") || !strings.Contains(err.Error(), `"small"`) {
			t.Fatalf("%s: the error should name what was taken out and keep short values: %v", call, err)
		}
	}
	if !errors.Is(createErr, provider.ErrNoOffer) {
		t.Fatalf("the redacted error no longer unwraps to the vendor's: %v", createErr)
	}
}
