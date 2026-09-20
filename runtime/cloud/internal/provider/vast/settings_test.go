package vast

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSettingsReadTheirSection(t *testing.T) {
	section := `
api_key_env: VAST_API_KEY
max_price_per_hour: 0.6
min_reliability: 0.98
regions: [SE, NL]
verified_only: true
accelerators:
  - product: NVIDIA GeForce RTX 4090
    gpu_name: RTX 4090
  - product: NVIDIA A100 80GB PCIe
    gpu_name: A100 PCIE
    min_gpu_ram_mb: 80000
`
	var cfg Settings
	if err := yaml.Unmarshal([]byte(section), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.APIKeyEnv != "VAST_API_KEY" || cfg.MaxPricePerHour != 0.6 || cfg.MinReliability != 0.98 || !cfg.VerifiedOnly || len(cfg.Regions) != 2 {
		t.Errorf("cfg = %+v", cfg)
	}
	a100, err := cfg.accelerator("nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIe")
	if err != nil || a100 != (Accelerator{Product: "NVIDIA A100 80GB PCIe", GPUName: "A100 PCIE", MinGPURAMMB: 80000}) {
		t.Errorf("accelerator = %+v, %v", a100, err)
	}
}

func TestNewRefusesIncompleteSettings(t *testing.T) {
	t.Setenv("VAST_TEST_KEY", testKey)
	t.Setenv("VAST_TEST_EMPTY", "")
	valid := Settings{APIKeyEnv: "VAST_TEST_KEY", MaxPricePerHour: 0.5, MinReliability: 0.9}
	cases := map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"no key reference":     {func(c *Settings) { c.APIKeyEnv = "" }, "api_key_env"},
		"key variable unset":   {func(c *Settings) { c.APIKeyEnv = "VAST_TEST_EMPTY" }, "VAST_TEST_EMPTY is not set"},
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
			if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
		})
	}
	if _, err := New(valid, nil); err != nil {
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
	t.Setenv("VAST_TEST_KEY", testKey)
	settings := Settings{
		APIKeyEnv:       "VAST_TEST_KEY",
		MaxPricePerHour: 0.5,
		Accelerators:    []Accelerator{{Product: "NVIDIA GeForce RTX 4090", GPUName: "RTX 4090"}},
	}

	if _, err := New(settings, []string{"nvidia.com/gpu.product=NVIDIA-GEFORCE-RTX-4090"}); err != nil {
		t.Fatalf("a type with an entry: %v", err)
	}
	_, err := New(settings, []string{"nvidia.com/gpu.product=NVIDIA-GEFORCE-RTX-4090", "nvidia.com/gpu.product=NVIDIA-H200"})
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-H200") {
		t.Fatalf("err = %v, want one naming the type with no entry", err)
	}
}
