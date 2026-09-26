package main

import (
	"flag"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/bare-metal/internal/hostinstall"
)

// The printed command of a newer image tag names no class. Run on a node that has one, it must
// leave the node's cores with the jobs that ask for them.
func TestAnInstallKeepsTheNodesCPUClassUnlessItNamesOne(t *testing.T) {
	installed := hostinstall.Settings{ClusterName: "box", CPUClass: "epyc-9354"}
	for name, tc := range map[string]struct {
		args      []string
		cluster   string
		installed hostinstall.Settings
		want      string
	}{
		"an upgrade that names none keeps it":              {nil, "box", installed, "epyc-9354"},
		"an upgrade that names another takes it":           {[]string{"--cpu-class", "genoa"}, "box", installed, "genoa"},
		"an upgrade that names none at all drops it":       {[]string{"--cpu-class", ""}, "box", installed, ""},
		"a node moved to another cluster starts with none": {nil, "other", installed, ""},
		"a first install with a class":                     {[]string{"--cpu-class", "genoa"}, "box", hostinstall.Settings{}, "genoa"},
		"a first install with none":                        {nil, "box", hostinstall.Settings{}, ""},
	} {
		flags := flag.NewFlagSet("install", flag.ContinueOnError)
		given := flags.String("cpu-class", "", "")
		if err := flags.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		if got := cpuClassFor(flags, tc.cluster, *given, tc.installed); got != tc.want {
			t.Errorf("%s: class = %q, want %q", name, got, tc.want)
		}
	}
}
