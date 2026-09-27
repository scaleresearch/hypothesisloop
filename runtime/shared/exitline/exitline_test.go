package exitline

import (
	"strings"
	"testing"
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

func TestLastOutputIsTheLastLineTheJobSaid(t *testing.T) {
	cases := map[string]struct {
		lines []string
		want  string
	}{
		"nothing":               {nil, ""},
		"only blank":            {[]string{"", "  \t"}, ""},
		"only the exit line":    {[]string{Format(1)}, ""},
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

func TestLastOutputCarriesTheErrorAboveTheAdvice(t *testing.T) {
	got := LastOutput([]string{
		`    batch = images[i : i + batch_size].to(DEVICE, non_blocking=True)`,
		`RuntimeError: CUDA error: misaligned address`,
		`CUDA kernel errors might be asynchronously reported at some other API call, so the stacktrace below might be incorrect.`,
		`For debugging consider passing CUDA_LAUNCH_BLOCKING=1`,
		"Compile with `TORCH_USE_CUDA_DSA` to enable device-side assertions.",
		"", "", Format(1),
	})
	want := "RuntimeError: CUDA error: misaligned address | CUDA kernel errors might be asynchronously reported at some other API call, so the stacktrace below might be incorrect. | For debugging consider passing CUDA_LAUNCH_BLOCKING=1 | Compile with `TORCH_USE_CUDA_DSA` to enable device-side assertions."
	if got != want {
		t.Fatalf("LastOutput = %q, want %q", got, want)
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
