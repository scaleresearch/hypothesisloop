package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

// PrintLeading prints record as PrintJSON does with the keys lead ahead of the others: its
// description is long, and a reader of the first lines must not miss what it waits for.
func PrintLeading(stdout io.Writer, record map[string]any, lead ...string) {
	keys := make([]string, 0, len(record))
	for _, key := range lead {
		if _, has := record[key]; has {
			keys = append(keys, key)
		}
	}
	rest := make([]string, 0, len(record))
	for key := range record {
		if !slices.Contains(lead, key) {
			rest = append(rest, key)
		}
	}
	slices.Sort(rest)
	var b bytes.Buffer
	b.WriteString("{")
	for i, key := range append(keys, rest...) {
		name, _ := json.Marshal(key)
		value, err := json.MarshalIndent(record[key], "  ", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "hl: encoding response: %s\n", err)
			return
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n  %s: %s", name, value)
	}
	b.WriteString("\n}")
	fmt.Fprintln(stdout, b.String())
}

// RefusalOf is the reason and the message err was refused with by the platform, empty for any
// other error.
func RefusalOf(err error) (reason, message string) {
	var refused *apiclient.HTTPError
	var body platformapi.ErrorModel
	if !errors.As(err, &refused) || json.Unmarshal([]byte(refused.Body), &body) != nil {
		return "", ""
	}
	return body.Reason, body.Message
}

func PrintJSON(stdout io.Writer, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hl: encoding response: %s\n", err)
		return
	}
	fmt.Fprintln(stdout, string(data))
}

// Fail prints err on one line, a JSON reply of the platform compacted like any other output.
func Fail(stderr io.Writer, err error) int {
	var refused *apiclient.HTTPError
	if errors.As(err, &refused) && refused.Status == http.StatusForbidden {
		fmt.Fprintf(stderr, "hl: the platform refuses this token %s %s: %s\n", refused.Method, strings.SplitN(refused.Path, "?", 2)[0], refusalMessage([]byte(refused.Body)))
		return 1
	}
	if errors.As(err, &refused) {
		if problems := problemLines([]byte(refused.Body)); problems != "" {
			refused.Body = problems
		} else if body, jsonErr := CompactJSON([]byte(refused.Body)); jsonErr == nil {
			refused.Body = string(body)
		}
	}
	fmt.Fprintf(stderr, "hl: %s\n", err)
	return 1
}

// refusalMessage is the platform's own words in a refusal's body, or the body as it is.
func refusalMessage(body []byte) string {
	var refusal platformapi.ErrorModel
	if json.Unmarshal(body, &refusal) == nil && refusal.Message != "" {
		return refusal.Message
	}
	return strings.TrimSpace(string(body))
}

// problemLines is a refusal that holds several problems, one line each, reason first, so all of
// them are fixed in one pass. Empty for an answer with fewer than two.
func problemLines(body []byte) string {
	var refusal platformapi.ErrorModel
	if json.Unmarshal(body, &refusal) != nil || len(refusal.Problems) < 2 {
		return ""
	}
	lines := []string{fmt.Sprintf("%d problems, fix them all:", len(refusal.Problems))}
	for _, p := range refusal.Problems {
		lines = append(lines, "  "+p.Reason+": "+p.Message)
	}
	return strings.Join(lines, "\n")
}

// PrintLines prints a JSON response compactly: an array one element per line, anything else on
// one line, each through compactJSON. An empty array prints none to stderr, so that no output is
// never mistaken for a command that did nothing.
func PrintLines(stdout, stderr io.Writer, none string, raw json.RawMessage, drop ...string) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return fmt.Errorf("response is not JSON: %w", err)
		}
		if len(items) == 0 {
			fmt.Fprintf(stderr, "hl: %s\n", none)
		}
		for _, item := range items {
			if err := PrintLine(stdout, item, drop...); err != nil {
				return err
			}
		}
		return nil
	}
	return PrintLine(stdout, raw, drop...)
}

func PrintLine(stdout io.Writer, raw json.RawMessage, drop ...string) error {
	line, err := CompactJSON(raw, drop...)
	if err != nil {
		return fmt.Errorf("response is not JSON: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "%s\n", line)
	return err
}

// CompactJSON re-encodes raw on one line in its own key order, leaving out every null,
// "" and {} value, and the named keys of the top-level object.
func CompactJSON(raw []byte, drop ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b bytes.Buffer
	if err := writeValue(dec, &b, drop); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeValue(dec *json.Decoder, b *bytes.Buffer, drop []string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			b.WriteByte('[')
			for i := 0; dec.More(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := writeValue(dec, b, nil); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		} else {
			b.WriteByte('{')
			n := 0
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := keyTok.(string)
				var v bytes.Buffer
				if err := writeValue(dec, &v, nil); err != nil {
					return err
				}
				if s := v.String(); slices.Contains(drop, key) || s == "null" || s == `""` || s == "{}" {
					continue
				}
				if n > 0 {
					b.WriteByte(',')
				}
				WriteJSONString(b, key)
				b.WriteByte(':')
				b.Write(v.Bytes())
				n++
			}
			b.WriteByte('}')
		}
		_, err := dec.Token()
		return err
	case string:
		WriteJSONString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case nil:
		b.WriteString("null")
	}
	return nil
}

func WriteJSONString(b *bytes.Buffer, s string) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Truncate(b.Len() - 1)
}

// Call makes one request and prints its reply, an object or the list under listKey, one compact
// line each. An answer that holds nothing, no body or an empty list, says none on stderr, so that
// no output is never mistaken for a command that did nothing. A route may answer a change with
// the resource or with 204, and either is the command done.
func Call(c *apiclient.Client, method, path string, body any, listKey, none string, stdout, stderr io.Writer) int {
	var out json.RawMessage
	if err := c.Do(method, path, body, &out); err != nil {
		return Fail(stderr, err)
	}
	if len(out) == 0 || string(out) == "null" {
		if none == "" {
			none = method + " " + path + ": done"
		}
		fmt.Fprintf(stderr, "hl: %s\n", none)
		return 0
	}
	if listKey != "" {
		var keyed map[string]json.RawMessage
		if err := json.Unmarshal(out, &keyed); err != nil {
			return Fail(stderr, fmt.Errorf("response is not a JSON object: %w", err))
		}
		out = keyed[listKey]
	}
	if err := PrintLines(stdout, stderr, none, out); err != nil {
		return Fail(stderr, err)
	}
	return 0
}

// PageNotice says on stderr that a list was cut: how many of the total this page shows and the
// --offset of the next one. total is the platform's X-Total-Count.
func PageNotice(stderr io.Writer, label string, header http.Header, offset, shown int) {
	if total, _ := strconv.Atoi(header.Get("X-Total-Count")); total > offset+shown {
		fmt.Fprintf(stderr, "hl: %s: %d of %d shown: --offset %d for the next page\n", label, shown, total, offset+shown)
	}
}

// CallPage reads a page of a list and prints one line per row, then says if more match.
func CallPage(c *apiclient.Client, path, label string, offset int, stdout, stderr io.Writer) int {
	var out json.RawMessage
	header, err := c.DoHeader("GET", path, nil, &out)
	if err != nil {
		return Fail(stderr, err)
	}
	if err := PrintLines(stdout, stderr, label+": none", out); err != nil {
		return Fail(stderr, err)
	}
	var rows []json.RawMessage
	_ = json.Unmarshal(out, &rows)
	PageNotice(stderr, label, header, offset, len(rows))
	return 0
}

// PageFlags are --limit and --offset of a list the platform pages, and the offset asked for.
func PageFlags(fs *Flags) (func() string, *int) {
	limit := fs.Int("limit", 0, "page size (default the platform's 20, at most 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	return func() string {
		query := url.Values{}
		if *limit != 0 {
			query.Set("limit", fmt.Sprint(*limit))
		}
		if *offset != 0 {
			query.Set("offset", fmt.Sprint(*offset))
		}
		if len(query) == 0 {
			return ""
		}
		return "?" + query.Encode()
	}, offset
}
