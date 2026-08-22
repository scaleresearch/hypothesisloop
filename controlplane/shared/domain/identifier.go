package domain

import (
	"fmt"
	"regexp"
)

// identifierPattern is the shape every id the platform accepts from outside must have: RFC1123,
// lowercase alphanumeric and dashes, no leading or trailing dash.
//
// It exists because these ids are not just database keys. They are pasted verbatim into names and
// patterns that other systems interpret — Kubernetes object names, and object-store prefixes that
// become IAM Resource and StringLike patterns. IAM treats '*' and '?' as wildcards ANYWHERE in a
// pattern, so an agent that registered as "agent-*" would be handed a session policy whose write
// resource read "<pe>/agent-*/<job>/*" and could write into every other agent's prefix. Rejecting
// the character at the door is the only place that costs nothing; every layer below has to
// assume its input is already safe.
var identifierPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// MaxIdentifierLength bounds an id at the DNS label limit Kubernetes object names are built from.
const MaxIdentifierLength = 63

// ValidateIdentifier reports whether value is usable as an externally-supplied id, naming the
// field in the error so the submitter can act on it. kind is the field name as the API spells it.
func ValidateIdentifier(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if len(value) > MaxIdentifierLength {
		return fmt.Errorf("%s must be at most %d characters, got %d", kind, MaxIdentifierLength, len(value))
	}
	if !identifierPattern.MatchString(value) {
		return fmt.Errorf(
			"%s %q must be lowercase alphanumeric or '-', starting and ending alphanumeric: it is used verbatim in Kubernetes object names and in object-store access policies, where a character like '*' would silently widen what the job may reach",
			kind, value)
	}
	return nil
}
