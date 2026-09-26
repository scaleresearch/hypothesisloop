package domain

import (
	"strings"
	"testing"
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
