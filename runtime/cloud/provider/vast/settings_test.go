package vast

import (
	"strings"
	"testing"
)

func TestNewRefusesIncompleteSettings(t *testing.T) {
	valid := Settings{AccountID: "1", MaxPricePerHour: 0.5, MinReliability: 0.9}
	if _, err := New(valid, "", nil, t.Logf); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Errorf("no key: err = %v, want it refused", err)
	}
	cases := map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"no account":           {func(c *Settings) { c.AccountID = "" }, "account_id"},
		"no price ceiling":     {func(c *Settings) { c.MaxPricePerHour = 0 }, "max_price_per_hour"},
		"reliability above 1":  {func(c *Settings) { c.MinReliability = 1.5 }, "min_reliability"},
		"accelerator unnamed":  {func(c *Settings) { c.Accelerators = []Accelerator{{Product: "NVIDIA H200"}} }, "gpu_name"},
		"accelerator unmapped": {func(c *Settings) { c.Accelerators = []Accelerator{{GPUName: "H200"}} }, "product"},
		"negative reliability": {func(c *Settings) { c.MinReliability = -0.1 }, "min_reliability"},
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

func TestErrorsNeverCarryTheAPIKey(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, Settings{})
	p.baseURL = "http://127.0.0.1:1"

	_, err := p.List(t.Context(), "hl-")
	if err == nil {
		t.Fatal("no error from an unreachable API")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error carries the key: %v", err)
	}
}

func TestNewRefusesAnAcceleratorTypeTheSettingsCannotRent(t *testing.T) {
	settings := Settings{
		MaxPricePerHour: 0.5,
		Accelerators:    []Accelerator{{Product: "NVIDIA GeForce RTX 4090", GPUName: "RTX 4090"}},
	}

	if _, err := New(settings, testKey, []string{"nvidia.com/gpu.product=NVIDIA-GEFORCE-RTX-4090"}, t.Logf); err != nil {
		t.Fatalf("a type with an entry: %v", err)
	}
	_, err := New(settings, testKey, []string{"nvidia.com/gpu.product=NVIDIA-GEFORCE-RTX-4090", "nvidia.com/gpu.product=NVIDIA-H200"}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-H200") {
		t.Fatalf("err = %v, want one naming the type with no entry", err)
	}
}
