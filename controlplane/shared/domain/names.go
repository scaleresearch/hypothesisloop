package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameLength bounds, in bytes, every name a person gives or carries: an org, a project, an
// API key, and the name their identity provider states for them.
const MaxNameLength = 100

// ValidateName returns a display name trimmed of surrounding space, or says why it is none. A
// name is shown in terminals and pages and stored as text, so it holds no control character and
// is valid UTF-8.
func ValidateName(what, name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%s name is required", what)
	case len(name) > MaxNameLength:
		return "", fmt.Errorf("%s name is longer than %d bytes", what, MaxNameLength)
	case !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%s name holds a control character or is not valid UTF-8", what)
	}
	return name, nil
}
