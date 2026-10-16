package domain

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxEventDetailJSONBytes bounds a free-text detail as JSON escapes it. Every event is sent with
// pg_notify inside the transaction that writes it, and Postgres aborts that transaction for a
// payload of 8000 bytes or more: a FAILED job whose last output was escape sequences grew past it
// and its end could not be recorded. The rest of an event is ids, codes and numbers.
const MaxEventDetailJSONBytes = 4096

const eventDetailTruncated = "… [truncated]"

// EventDetail is text as it may be stored where an event carries it as its detail. Control
// characters are left out but newline and tab, as a detail is shown in lines, invalid UTF-8 is
// replaced, and text that would escape to more than MaxEventDetailJSONBytes of JSON is cut and
// ends with eventDetailTruncated.
func EventDetail(text string) string {
	kept := make([]rune, 0, len(text))
	size := 0
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		kept = append(kept, r)
		size += jsonRuneBytes(r)
	}
	if size <= MaxEventDetailJSONBytes {
		return string(kept)
	}
	var b strings.Builder
	budget := MaxEventDetailJSONBytes - len(eventDetailTruncated)
	for _, r := range kept {
		if budget -= jsonRuneBytes(r); budget < 0 {
			break
		}
		b.WriteRune(r)
	}
	b.WriteString(eventDetailTruncated)
	return b.String()
}

// jsonRuneBytes is what r takes in a JSON string, as Postgres or Go without HTML escaping writes
// it, for every rune EventDetail keeps.
func jsonRuneBytes(r rune) int {
	switch r {
	case '"', '\\', '\n', '\t':
		return 2
	case ' ', ' ':
		return 6
	}
	return utf8.RuneLen(r)
}
