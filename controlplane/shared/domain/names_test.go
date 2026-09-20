package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateNameRefusesWhatCannotBeShownOrStored(t *testing.T) {
	for name, text := range map[string]string{
		"empty": "", "only space": "  \t ", "a NUL byte": "pro\x00ject", "a newline": "two\nlines", "an escape": "red\x1b[31m",
		"invalid UTF-8": "caf\xe9", "too long": strings.Repeat("a", MaxNameLength+1),
	} {
		if got, err := ValidateName("project", text); err == nil {
			t.Errorf("%s: ValidateName = %q, nil, want an error", name, got)
		}
	}
	for text, want := range map[string]string{"  vision  ": "vision", "Zoë's lab": "Zoë's lab", strings.Repeat("a", MaxNameLength): strings.Repeat("a", MaxNameLength)} {
		if got, err := ValidateName("project", text); err != nil || got != want {
			t.Errorf("ValidateName(%q) = %q, %v, want %q", text, got, err, want)
		}
	}
}

func TestNameFromAlwaysYieldsAValidNameOrNothing(t *testing.T) {
	for _, text := range []string{
		"Ada Lovelace", "  padded  ", "nul\x00byte", "line\nbreak", "caf\xe9", strings.Repeat("é", 400), strings.Repeat("a", 99) + "é", "\x00\x01", "",
	} {
		got := NameFrom(text)
		if got == "" {
			continue
		}
		if cleaned, err := ValidateName("org", got); err != nil || cleaned != got {
			t.Errorf("NameFrom(%q) = %q, which ValidateName answers with %q, %v", text, got, cleaned, err)
		}
	}
	if got := NameFrom(strings.Repeat("é", 400)); len(got) != MaxNameLength || !utf8.ValidString(got) {
		t.Errorf("a long name is cut to %d bytes of %q, want %d bytes on a character boundary", len(got), got, MaxNameLength)
	}
	if got := NameFrom("\x00\x01 "); got != "" {
		t.Errorf("NameFrom of control characters alone = %q, want nothing", got)
	}
	if got := NameFrom("Ada\x00 Lovelace"); got != "Ada Lovelace" {
		t.Errorf("NameFrom drops what is refused and keeps the rest: got %q", got)
	}
}
