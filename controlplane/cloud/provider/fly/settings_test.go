package fly

import (
	"strings"
	"testing"
)

func TestNewRefusesIncompleteSettings(t *testing.T) {
	valid := Settings{App: testApp, Region: "ams", CPUKind: "performance"}
	for token, want := range map[string]string{"": `starting with "FlyV1 "`, "fm2_no-scheme": `starting with "FlyV1 "`} {
		if _, err := New(valid, token, nil, t.Logf); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("token %q: err = %v, want one about %q", token, err, want)
		}
	}
	cases := map[string]struct {
		mutate func(*Settings)
		want   string
	}{
		"no app":             {func(c *Settings) { c.App = "" }, "app"},
		"app that is a path": {func(c *Settings) { c.App = "a/../b" }, "app"},
		"no region":          {func(c *Settings) { c.Region = "" }, "region"},
		"region by its name": {func(c *Settings) { c.Region = "Amsterdam" }, "region"},
		"no cpu kind":        {func(c *Settings) { c.CPUKind = "" }, "cpu_kind"},
		"unknown cpu kind":   {func(c *Settings) { c.CPUKind = "burstable" }, "cpu_kind"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := New(cfg, testToken, nil, t.Logf); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
		})
	}
	if _, err := New(valid, testToken, nil, t.Logf); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
}

func TestNewRefusesAnyAcceleratorType(t *testing.T) {
	valid := Settings{App: testApp, Region: "ams", CPUKind: "shared"}

	_, err := New(valid, testToken, []string{"nvidia.com/gpu.product=NVIDIA-H200"}, t.Logf)
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-H200") {
		t.Fatalf("err = %v, want one naming the accelerator type a shape asks for", err)
	}
}

func TestErrorsNeverCarryTheAPIToken(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)
	p.machinesURL = "http://127.0.0.1:1"

	_, err := p.List(t.Context(), "hl-")
	if err == nil {
		t.Fatal("no error from an unreachable API")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Errorf("error carries the token: %v", err)
	}
}
