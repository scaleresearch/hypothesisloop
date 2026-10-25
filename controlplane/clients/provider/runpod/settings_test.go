package runpod

import (
	"strings"
	"testing"
)

func TestNewRefusesIncompleteSettings(t *testing.T) {
	valid := testSettings()
	if _, err := New(valid, "", nil, t.Logf); err == nil || !strings.Contains(err.Error(), "API key is required") {
		t.Errorf("no key: err = %v, want it refused", err)
	}
	cases := map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"no account":          {func(c *Settings) { c.AccountID = "" }, "account_id"},
		"no price ceiling":    {func(c *Settings) { c.MaxPricePerHour = 0 }, "max_price_per_hour"},
		"no cloud":            {func(c *Settings) { c.Cloud = "" }, "cloud"},
		"another cloud":       {func(c *Settings) { c.Cloud = "secure" }, "cloud"},
		"accelerator unnamed": {func(c *Settings) { c.Accelerators = map[string]Accelerator{h200: {}} }, "gpu_id"},
		"negative ceiling": {func(c *Settings) {
			c.Accelerators = map[string]Accelerator{h200: {GPUID: "NVIDIA H200", MaxPricePerGPUHour: -1}}
		}, "max_price_per_gpu_hour"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := New(cfg, testKey, cfg.Accelerators, t.Logf); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
		})
	}
	if _, err := New(valid, testKey, valid.Accelerators, t.Logf); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
}

const h200 = "nvidia.com/gpu.product=NVIDIA-H200"
