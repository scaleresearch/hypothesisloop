package modelproxy

import (
	"strings"
	"testing"
)

// A stream arrives in reads that split its lines anywhere.
func TestAStreamsUsageIsReadWhereverItsReadsSplitIt(t *testing.T) {
	stream := messageStart + textDelta + messageDelta + messageStop
	want := usage{InputTokens: 12, OutputTokens: 42, CacheCreationInputTokens: 300, CacheReadInputTokens: 4000}
	for _, size := range []int{1, 3, 64, len(stream)} {
		r := newUsageReader("text/event-stream; charset=utf-8")
		for start := 0; start < len(stream); start += size {
			r.Write([]byte(stream[start:min(start+size, len(stream))]))
		}
		if got, ok := r.Usage(); !ok || got != want {
			t.Errorf("reads of %d bytes: usage = %+v, %v, want %+v", size, got, ok, want)
		}
	}
}

// The final message_delta states every count it carries cumulatively, and replaces only those.
func TestAMessageDeltaReplacesTheCountsItStates(t *testing.T) {
	r := newUsageReader("text/event-stream")
	r.Write([]byte(strings.ReplaceAll(messageStart, "\n", "\r\n")))
	r.Write([]byte("event: message_delta\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":15,\"cache_read_input_tokens\":4100,\"output_tokens\":9}}\r\n\r\n"))
	want := usage{InputTokens: 15, OutputTokens: 9, CacheCreationInputTokens: 300, CacheReadInputTokens: 4100}
	if got, ok := r.Usage(); !ok || got != want {
		t.Fatalf("usage = %+v, %v, want %+v", got, ok, want)
	}
}

func TestAStreamCutBeforeItsStartStatesNoUsage(t *testing.T) {
	r := newUsageReader("text/event-stream")
	r.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\nevent: message_st"))
	if got, ok := r.Usage(); ok {
		t.Fatalf("usage = %+v, want none", got)
	}
}

// A line longer than any usage event is skipped to its end, and what follows is still read.
func TestALineTooLongToKeepIsSkipped(t *testing.T) {
	r := newUsageReader("text/event-stream")
	r.Write([]byte(messageStart))
	r.Write([]byte("data: {\"type\":\"content_block_delta\",\"text\":\"" + strings.Repeat("x", maxEventLine) + "\"}\n\n"))
	r.Write([]byte(messageDelta))
	if got, ok := r.Usage(); !ok || got.OutputTokens != 42 || got.InputTokens != 12 {
		t.Fatalf("usage = %+v, %v", got, ok)
	}
	if cap(r.line) > 2*maxEventLine {
		t.Fatalf("kept %d bytes of a skipped line", cap(r.line))
	}
}

func TestAnAnswerThatIsNoMessageStatesNoUsage(t *testing.T) {
	for _, body := range []string{`{"type":"error","error":{"type":"overloaded_error"}}`, `not json`, `{"input_tokens":5}`} {
		r := newUsageReader("application/json")
		r.Write([]byte(body))
		if got, ok := r.Usage(); ok {
			t.Errorf("%s: usage = %+v, want none", body, got)
		}
	}
}

// OpenRouter states the call's cost in its final message_delta, and null for a count it does not
// make, which leaves the count as it was.
func TestOpenRoutersStatedCostIsRead(t *testing.T) {
	for name, c := range map[string]struct {
		contentType, answer string
	}{
		"streamed": {"text/event-stream", openRouterStart + textDelta + openRouterDelta + messageStop},
		"whole":    {"application/json", `{"type":"message","model":"moonshotai/kimi-k2","usage":{"input_tokens":8,"output_tokens":125,"cache_creation_input_tokens":null,"cache_read_input_tokens":0,"cost":0.0000504}}`},
	} {
		r := newUsageReader(c.contentType)
		r.Write([]byte(c.answer))
		got, ok := r.Usage()
		if !ok || got.InputTokens != 8 || got.OutputTokens != 125 || got.CacheCreationInputTokens != 0 || got.CostUSD == nil || *got.CostUSD != 0.0000504 {
			t.Errorf("%s: usage = %+v, %v", name, got, ok)
		}
	}
	r := newUsageReader("text/event-stream")
	r.Write([]byte(messageStart + messageDelta))
	if got, _ := r.Usage(); got.CostUSD != nil {
		t.Errorf("an answer stating no cost: cost = %v, want none", *got.CostUSD)
	}
}
