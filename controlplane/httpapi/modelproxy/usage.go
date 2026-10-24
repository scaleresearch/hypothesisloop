package modelproxy

import (
	"bytes"
	"encoding/json"
	"mime"
)

// usage is what the provider counted for one call, and its cost in US dollars when the provider
// states one, as OpenRouter does.
type usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	CostUSD                  *float64
}

// counts is a usage block as the provider writes it. A field it leaves out or states null is nil,
// so a later block replaces only what it states.
type counts struct {
	InputTokens              *int64   `json:"input_tokens"`
	OutputTokens             *int64   `json:"output_tokens"`
	CacheCreationInputTokens *int64   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64   `json:"cache_read_input_tokens"`
	Cost                     *float64 `json:"cost"`
}

func (u *usage) take(c *counts) {
	if c == nil {
		return
	}
	for _, field := range []struct {
		to   *int64
		from *int64
	}{{&u.InputTokens, c.InputTokens}, {&u.OutputTokens, c.OutputTokens}, {&u.CacheCreationInputTokens, c.CacheCreationInputTokens}, {&u.CacheReadInputTokens, c.CacheReadInputTokens}} {
		if field.from != nil {
			*field.to = *field.from
		}
	}
	if c.Cost != nil {
		u.CostUSD = c.Cost
	}
}

const (
	// maxEventLine bounds one line of a streamed answer kept while it is incomplete. The events
	// that carry usage are short; a longer line is skipped to its end.
	maxEventLine = 1 << 20
	// maxBody bounds an answer that is not streamed, which is read whole for its usage.
	maxBody = 8 << 20
)

// usageReader reads the usage of an answer from its bytes as they pass. A streamed answer
// (text/event-stream) states the input counts in its message_start event and the final counts,
// cumulative, and OpenRouter's cost, in its message_delta events. Any other answer is one message, or
// the error a failed answer of the ChatGPT backend states its usage beside, whose usage is read once
// it is whole.
type usageReader struct {
	stream bool
	// atEnd is true of a provider whose message_start states no counts yet: its stream states its
	// usage in its message_delta alone.
	atEnd bool
	// line is the incomplete line of a stream, body the answer that is not streamed.
	line, body []byte
	skipping   bool
	seen       bool
	ended      bool
	usage      usage
}

func newUsageReader(contentType string, atEnd bool) *usageReader {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return &usageReader{stream: mediaType == "text/event-stream", atEnd: atEnd}
}

func (r *usageReader) Write(b []byte) {
	if !r.stream {
		if len(r.body)+len(b) <= maxBody {
			r.body = append(r.body, b...)
		}
		return
	}
	for len(b) > 0 {
		end := bytes.IndexByte(b, '\n')
		if end < 0 {
			if !r.skipping && len(r.line)+len(b) <= maxEventLine {
				r.line = append(r.line, b...)
			} else {
				r.line, r.skipping = r.line[:0], true
			}
			return
		}
		if !r.skipping && len(r.line)+end <= maxEventLine {
			r.event(append(r.line, b[:end]...))
		}
		r.line, r.skipping = r.line[:0], false
		b = b[end+1:]
	}
}

func (r *usageReader) event(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"message_`)) {
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message struct {
			Usage *counts `json:"usage"`
		} `json:"message"`
		Usage *counts `json:"usage"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	switch event.Type {
	case "message_start":
		r.seen = true
		r.usage.take(event.Message.Usage)
	case "message_delta":
		r.ended = r.ended || event.Usage != nil
		r.usage.take(event.Usage)
	}
}

// Usage is the usage read so far, and whether the answer stated any: a stream cut before its
// message_start, one of a provider stating usage at its end that ended before its message_delta,
// or a body that is no message and no error stating usage, states none.
func (r *usageReader) Usage() (usage, bool) {
	if r.stream && r.atEnd {
		return r.usage, r.ended
	}
	if r.stream {
		return r.usage, r.seen
	}
	var message struct {
		Type  string  `json:"type"`
		Usage *counts `json:"usage"`
	}
	if json.Unmarshal(r.body, &message) != nil || (message.Type != "message" && message.Type != "error") || message.Usage == nil {
		return usage{}, false
	}
	var u usage
	u.take(message.Usage)
	return u, true
}
