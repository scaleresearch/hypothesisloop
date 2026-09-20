package provider

import (
	"strings"
	"testing"
)

func validSpec() Spec {
	return Spec{Name: "hl-job-main-e1-a2", Image: "registry.example/job:1", Command: []string{"hl", "exec"},
		Env: map[string]string{"HL_TOKEN": "t", "_x1": ""}, CPUMillis: 1, MemoryBytes: 1, DiskBytes: 1}
}

func TestSpecValidateAcceptsACompleteSpec(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatal(err)
	}
	gpu := validSpec()
	gpu.AcceleratorCount, gpu.AcceleratorType = 2, "nvidia.com/gpu.product=NVIDIA-H100"
	if err := gpu.Validate(); err != nil {
		t.Fatalf("a spec with accelerators: %v", err)
	}
}

func TestSpecValidateRefusesWhatNoVendorCanRent(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Spec)
		want   string
	}{
		"no name":                {func(s *Spec) { s.Name = "" }, "instance name"},
		"name outside the set":   {func(s *Spec) { s.Name = "HL job" }, "instance name"},
		"no image":               {func(s *Spec) { s.Image = "" }, "no image"},
		"no command":             {func(s *Spec) { s.Command = nil }, "no command"},
		"empty executable":       {func(s *Spec) { s.Command = []string{"", "x"} }, "no command"},
		"negative accelerators":  {func(s *Spec) { s.AcceleratorCount = -1 }, "-1 accelerators"},
		"no cpu":                 {func(s *Spec) { s.CPUMillis = 0 }, "cpu, memory and disk"},
		"no memory":              {func(s *Spec) { s.MemoryBytes = 0 }, "cpu, memory and disk"},
		"no disk":                {func(s *Spec) { s.DiskBytes = 0 }, "cpu, memory and disk"},
		"an option as env name":  {func(s *Spec) { s.Env = map[string]string{"-p 22:22": "1"} }, "not an environment variable name"},
		"a command as env name":  {func(s *Spec) { s.Env = map[string]string{"A=1; rm -rf /; B": "1"} }, "not an environment variable name"},
		"an env name with digit": {func(s *Spec) { s.Env = map[string]string{"1A": "1"} }, "not an environment variable name"},
	}
	for name, tc := range cases {
		spec := validSpec()
		tc.mutate(&spec)
		if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one about %q", name, err, tc.want)
		}
	}
}

func TestCeilDivRoundsUp(t *testing.T) {
	for _, tc := range [][3]int64{{1, 1000, 1}, {1000, 1000, 1}, {1001, 1000, 2}, {8 << 30, 1 << 30, 8}, {8<<30 + 1, 1 << 30, 9}} {
		if got := CeilDiv(tc[0], tc[1]); got != tc[2] {
			t.Errorf("CeilDiv(%d, %d) = %d, want %d", tc[0], tc[1], got, tc[2])
		}
	}
}
