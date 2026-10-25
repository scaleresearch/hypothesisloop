package e2b

import (
	"strings"
	"testing"
)

func TestNewRefusesIncompleteSettings(t *testing.T) {
	valid := Settings{LifetimeSeconds: 3600}
	if _, err := New(valid, "", nil, t.Logf); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Errorf("no key: err = %v, want it refused", err)
	}
	cases := map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"no lifetime":       {func(c *Settings) { c.LifetimeSeconds = 0 }, "lifetime_seconds"},
		"negative lifetime": {func(c *Settings) { c.LifetimeSeconds = -1 }, "lifetime_seconds"},
		"a lifetime no longer than the notice of its end": {func(c *Settings) { c.LifetimeSeconds = 300 }, "lifetime_seconds must be above 300"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := New(cfg, testKey, nil, t.Logf); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
		})
	}
	if _, err := New(valid, testKey, nil, t.Logf); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
}

func TestNewRefusesAnyAcceleratorType(t *testing.T) {
	_, err := New(Settings{LifetimeSeconds: 3600}, testKey, map[string]struct{}{"nvidia.com/gpu.product=NVIDIA-H200": {}}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-H200") {
		t.Fatalf("err = %v, want one naming the accelerator type a shape asks for", err)
	}
}

func TestErrorsNeverCarryTheAPIKey(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	p.baseURL = "http://127.0.0.1:1"

	_, err := p.List(t.Context(), "hl-")
	if err == nil {
		t.Fatal("no error from an unreachable API")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error carries the key: %v", err)
	}
}
