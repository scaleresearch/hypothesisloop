package exitline

import (
	"testing"
	"time"
)

func TestFindReadsBackWhatFormatWrote(t *testing.T) {
	for _, code := range []int{0, 1, 137} {
		got, found := Find([]string{"epoch 1", Format(code)})
		if !found || got != code {
			t.Errorf("Find after Format(%d) = %d, %v", code, got, found)
		}
	}
}

func TestFindTakesTheLastExitLine(t *testing.T) {
	lines := []string{Format(0), "the job printed the line above itself", Format(3), "trailing stderr"}
	if code, found := Find(lines); !found || code != 3 {
		t.Fatalf("Find = %d, %v, want 3", code, found)
	}
}

func TestFindReportsOutputWithNoExitLine(t *testing.T) {
	for _, lines := range [][]string{nil, {"epoch 1", "Killed"}, {"hl-exec: exit-code=abc"}} {
		if code, found := Find(lines); found {
			t.Errorf("Find(%q) = %d, found", lines, code)
		}
	}
}

func TestFindRunReadsBackWhatFormatRunWrote(t *testing.T) {
	started := time.Date(2026, 10, 6, 21, 42, 50, 123e6, time.UTC)
	ended := started.Add(12345 * time.Millisecond)
	forged := FormatRun(started.Add(-time.Hour), ended)
	gotStart, gotEnd, found := FindRun([]string{forged, "epoch 1", FormatRun(started, ended), Format(0)})
	if !found || !gotStart.Equal(started) || !gotEnd.Equal(ended) {
		t.Fatalf("FindRun = %v, %v, %v", gotStart, gotEnd, found)
	}
	for _, lines := range [][]string{nil, {Format(0)}, {"hl-exec: ran yesterday today"}, {"hl-exec: ran 2026-10-06T21:42:50.123Z"}} {
		if _, _, found := FindRun(lines); found {
			t.Errorf("FindRun(%q) found a run", lines)
		}
	}
}
