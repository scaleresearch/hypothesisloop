package e2b

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSettingsReadTheirSection(t *testing.T) {
	section := `
api_key_env: E2B_API_KEY
lifetime_seconds: 86400
`
	var cfg Settings
	if err := yaml.Unmarshal([]byte(section), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg != (Settings{APIKeyEnv: "E2B_API_KEY", LifetimeSeconds: 86400}) {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestNewRefusesIncompleteSettings(t *testing.T) {
	t.Setenv("E2B_TEST_KEY", testKey)
	t.Setenv("E2B_TEST_EMPTY", "")
	valid := Settings{APIKeyEnv: "E2B_TEST_KEY", LifetimeSeconds: 3600}
	cases := map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"no key reference":   {func(c *Settings) { c.APIKeyEnv = "" }, "api_key_env"},
		"key variable unset": {func(c *Settings) { c.APIKeyEnv = "E2B_TEST_EMPTY" }, "E2B_TEST_EMPTY is not set"},
		"no lifetime":        {func(c *Settings) { c.LifetimeSeconds = 0 }, "lifetime_seconds"},
		"negative lifetime":  {func(c *Settings) { c.LifetimeSeconds = -1 }, "lifetime_seconds"},
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

func TestNewRefusesAnyAcceleratorType(t *testing.T) {
	t.Setenv("E2B_TEST_KEY", testKey)

	_, err := New(Settings{APIKeyEnv: "E2B_TEST_KEY", LifetimeSeconds: 3600}, []string{"nvidia.com/gpu.product=NVIDIA-H200"})
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
