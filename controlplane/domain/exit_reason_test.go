package domain

import (
	"strings"
	"testing"
)

func TestExitReasonOf(t *testing.T) {
	for log, want := range map[string]string{
		"Traceback\nhttpx.ConnectError: [Errno -2] Name or service not known\n": "could not reach the platform",
		"OSError: [Errno 111] Connection refused":                               "could not reach the platform",
		"ValueError: bad token":                                                 "ValueError: bad token",
		strings.Repeat("x", 200):                                                strings.Repeat("x", 119) + "…",
	} {
		if got := ExitReasonOf(log); got != want {
			t.Errorf("ExitReasonOf(%q) = %q, want %q", log, got, want)
		}
	}
}
