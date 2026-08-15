package registry

import "testing"

func TestParseSampleValue(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
		ok   bool
	}{
		{"1.5", 1.5, true},
		{"0", 0, true},
		{"-2.25", -2.25, true},
		{"1e-3", 0.001, true},
		// Rejected: each would otherwise be recorded as a real 0, which on a minimized metric
		// reads as a perfect score.
		{"NaN", 0, false},
		{"+Inf", 0, false},
		{"-Inf", 0, false},
		{"not-a-number", 0, false},
		{"", 0, false},
		{float64(1.5), 0, false}, // Prometheus sends values as strings
		{nil, 0, false},
	} {
		got, ok := parseSampleValue(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseSampleValue(%#v) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
