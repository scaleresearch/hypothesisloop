package exitline

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestLastOutputIsTheLastLineTheJobSaid(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  string
	}{
		"nothing":               {nil, ""},
		"only blank":            {[]string{"", "  \t"}, ""},
		"only the exit line":    {[]string{Format(1)}, ""},
		"the wrapper's lines":   {[]string{"boom", FormatRun(time.Unix(1, 0), time.Unix(2, 0)), Format(1)}, "boom"},
		"before the exit line":  {[]string{"Traceback", "KeyError: 'lr'", Format(1), ""}, "Traceback | KeyError: 'lr'"},
		"after the exit line":   {[]string{"Traceback", Format(1), "late stderr"}, "Traceback | late stderr"},
		"made one line":         {[]string{"\x1b[31mfatal:\x1b[0m\tno  such\rfile\xff "}, "[31mfatal: [0m no such file"},
		"section header counts": {[]string{"--- current attempt (pod a) ---", "boom"}, "--- current attempt (pod a) --- | boom"},
		"the last five":         {[]string{"1", "2", "", "3", "4", "5", "6"}, "2 | 3 | 4 | 5 | 6"},
	}
	for name, c := range cases {
		if got := LastOutput(c.lines); got != c.want {
			t.Errorf("%s: LastOutput = %q, want %q", name, got, c.want)
		}
	}
}

func TestLastOutputIsCutOnACharacterBoundary(t *testing.T) {
	got := LastOutput([]string{strings.Repeat("é", 400)})
	if len(got) != lastOutputBytes || !utf8.ValidString(got) {
		t.Fatalf("LastOutput is %d bytes, valid %v, want %d valid bytes", len(got), utf8.ValidString(got), lastOutputBytes)
	}
	if got := LastOutput([]string{strings.Repeat("a", 299) + "é"}); got != strings.Repeat("a", 299) {
		t.Fatalf("a character that crosses the bound was not dropped whole: %d bytes", len(got))
	}
}

func TestLastOutputDropsOlderLinesThatDoNotFit(t *testing.T) {
	last := strings.Repeat("b", 200)
	if got := LastOutput([]string{"oldest", strings.Repeat("a", 200), last}); got != last {
		t.Fatalf("LastOutput is %d bytes, want the last line alone", len(got))
	}
	if got := LastOutput([]string{strings.Repeat("a", 97), last}); len(got) != lastOutputBytes {
		t.Fatalf("LastOutput is %d bytes, want both lines in exactly %d", len(got), lastOutputBytes)
	}
}
