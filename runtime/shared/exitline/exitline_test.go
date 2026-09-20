package exitline

import "testing"

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
