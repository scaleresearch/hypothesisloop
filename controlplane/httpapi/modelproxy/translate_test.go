package modelproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// translatedRequest is the Responses call of the Messages call body, as JSON decoded.
func translatedRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	out, _, err := toResponses([]byte(body), "gpt-6-luna", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAMessagesCallIsTheResponsesCallOfTheSameConversation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		field      string
		want       string
	}{
		{"a system prompt of blocks is the instructions",
			`{"model":"gpt-6-luna","system":[{"type":"text","text":"one","cache_control":{"type":"ephemeral"}},{"type":"text","text":"two"}],"messages":[]}`,
			"instructions", `"one\n\ntwo"`},
		{"a system prompt of a string is the instructions",
			`{"model":"gpt-6-luna","system":"be brief","messages":[]}`, "instructions", `"be brief"`},
		{"a user's text is input text",
			`{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`,
			"input", `[{"content":[{"text":"hi","type":"input_text"}],"role":"user","type":"message"}]`},
		{"a system message within the conversation is the developer's",
			`{"model":"gpt-6-luna","messages":[{"role":"system","content":[{"type":"text","text":"# Environment"}]}]}`,
			"input", `[{"content":[{"text":"# Environment","type":"input_text"}],"role":"developer","type":"message"}]`},
		{"an assistant's text is output text",
			`{"model":"gpt-6-luna","messages":[{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}`,
			"input", `[{"content":[{"text":"hello","type":"output_text"}],"role":"assistant","type":"message"}]`},
		{"a tool use is a function call and its result the function's output",
			`{"model":"gpt-6-luna","messages":[{"role":"assistant","content":[{"type":"text","text":"listing"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"a\nb"}]},{"type":"text","text":"go on"}]}]}`,
			"input", `[{"content":[{"text":"listing","type":"output_text"}],"role":"assistant","type":"message"},` +
				`{"arguments":"{\"command\":\"ls\"}","call_id":"toolu_1","name":"Bash","type":"function_call"},` +
				`{"call_id":"toolu_1","output":"a\nb","type":"function_call_output"},` +
				`{"content":[{"text":"go on","type":"input_text"}],"role":"user","type":"message"}]`},
		{"a tool result of a string is the function's output",
			`{"model":"gpt-6-luna","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`,
			"input", `[{"call_id":"toolu_1","output":"done","type":"function_call_output"}]`},
		{"a tool result that is an error says so in the output",
			`{"model":"gpt-6-luna","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"no such file","is_error":true}]}]}`,
			"input", `[{"call_id":"toolu_1","output":"Error: no such file","type":"function_call_output"}]`},
		{"an image a tool showed follows its output as the user's",
			`{"model":"gpt-6-luna","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}]}`,
			"input", `[{"call_id":"toolu_1","output":"","type":"function_call_output"},{"content":[{"image_url":"data:image/png;base64,AAA","type":"input_image"}],"role":"user","type":"message"}]`},
		{"thinking is reasoning carried in its encrypted content",
			`{"model":"gpt-6-luna","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"enc-1"},{"type":"thinking","thinking":"","signature":""},{"type":"text","text":"ok"}]}]}`,
			"input", `[{"encrypted_content":"enc-1","summary":[{"text":"plan","type":"summary_text"}],"type":"reasoning"},{"content":[{"text":"ok","type":"output_text"}],"role":"assistant","type":"message"}]`},
		{"a tool is a function",
			`{"model":"gpt-6-luna","messages":[],"tools":[{"name":"Bash","description":"runs","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]}`,
			"tools", `[{"description":"runs","name":"Bash","parameters":{"properties":{"command":{"type":"string"}},"type":"object"},"strict":false,"type":"function"}]`},
		{"the web_search server tool is OpenAI's web search, its sources included",
			`{"model":"gpt-6-luna","messages":[],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8,"allowed_domains":["arxiv.org"],"user_location":{"type":"approximate","country":"US"}}]}`,
			"tools", `[{"filters":{"allowed_domains":["arxiv.org"]},"type":"web_search","user_location":{"country":"US","type":"approximate"}}]`},
		{"a search keeps its sources", `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}],"thinking":{"type":"adaptive"}}`,
			"include", `["web_search_call.action.sources","reasoning.encrypted_content"]`},
		{"the search chosen is the web search", `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"tool","name":"web_search"}}`,
			"tool_choice", `{"type":"web_search"}`},
		{"any tool is a required one", `{"model":"gpt-6-luna","messages":[],"tool_choice":{"type":"any"}}`, "tool_choice", `"required"`},
		{"one tool is that function", `{"model":"gpt-6-luna","messages":[],"tool_choice":{"type":"tool","name":"Bash"}}`, "tool_choice", `{"name":"Bash","type":"function"}`},
		{"no parallel tool use is no parallel calls", `{"model":"gpt-6-luna","messages":[],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`, "parallel_tool_calls", `false`},
		{"adaptive thinking at an effort is reasoning summarized at it",
			`{"model":"gpt-6-luna","messages":[],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, "reasoning", `{"effort":"xhigh","summary":"auto"}`},
		{"thinking whose display is omitted is reasoning without a summary",
			`{"model":"gpt-6-luna","messages":[],"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"high"}}`, "reasoning", `{"effort":"high"}`},
		{"thinking keeps what was thought", `{"model":"gpt-6-luna","messages":[],"thinking":{"type":"enabled","budget_tokens":1024}}`, "include", `["reasoning.encrypted_content"]`},
		{"the call is streamed and stored nowhere", `{"model":"gpt-6-luna","messages":[],"stream":false,"max_tokens":5,"temperature":1}`, "store", `false`},
		{"the call names the model and the session", `{"model":"gpt-6-luna","messages":[]}`, "prompt_cache_key", `"session-1"`},
	} {
		got := translatedRequest(t, tc.body)
		if have := asJSON(t, got[tc.field]); have != tc.want {
			t.Errorf("%s: %s = %s, want %s", tc.name, tc.field, have, tc.want)
		}
		if got["stream"] != true || got["model"] != "gpt-6-luna" {
			t.Errorf("%s: stream %v, model %v, want a streamed call on gpt-6-luna", tc.name, got["stream"], got["model"])
		}
	}
}

func TestACallWithoutACounterpartIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"a server tool":       `{"model":"m","messages":[],"tools":[{"type":"code_execution_20250825","name":"code_execution"}]}`,
		"blocked domains":     `{"model":"m","messages":[],"tools":[{"type":"web_search_20250305","name":"web_search","blocked_domains":["x.com"]}]}`,
		"an unknown block":    `{"model":"m","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}]}`,
		"an unknown role":     `{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
		"a system image":      `{"model":"m","messages":[{"role":"system","content":[{"type":"image","source":{"type":"url","url":"u"}}]}]}`,
		"an unknown effort":   `{"model":"m","messages":[],"output_config":{"effort":"extreme"}}`,
		"no Messages request": `[1]`,
	} {
		if _, _, err := toResponses([]byte(body), "m", ""); err == nil {
			t.Errorf("%s: translated, want refused", name)
		}
	}
}

// sse is a Responses stream of events, each its JSON data.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var kind struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &kind)
		b.WriteString("event: " + kind.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

const (
	created   = `{"type":"response.created","response":{"id":"resp_1"}}`
	completed = `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":50}}}`
)

// anthropicEvents is each event of the Anthropic stream the Responses stream translates to,
// its data with the event's name checked against its type.
func anthropicEvents(t *testing.T, stream string, how shape) []string {
	t.Helper()
	translated, err := io.ReadAll(newTranslatedStream(io.NopCloser(strings.NewReader(stream)), how, zap.NewNop()))
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(string(translated)))
	scanner.Buffer(nil, 1<<20)
	var name string
	for scanner.Scan() {
		line := scanner.Text()
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			name = n
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var kind struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(data), &kind) != nil || kind.Type != name {
				t.Fatalf("event %s carries %s", name, data)
			}
			events = append(events, data)
		}
	}
	return events
}

func TestAResponsesStreamIsTheAnthropicStreamOfTheSameAnswer(t *testing.T) {
	start := `{"message":{"content":[],"id":"msg_resp_1","model":"gpt-6-luna","role":"assistant","stop_reason":null,"stop_sequence":null,"type":"message","usage":{"input_tokens":0,"output_tokens":0}},"type":"message_start"}`
	usage := `"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":600,"input_tokens":400,"output_tokens":50}`
	stop := `{"type":"message_stop"}`
	for _, tc := range []struct {
		name   string
		how    shape
		stream string
		want   []string
	}{
		{"text", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_a","type":"message"}}`,
			`{"type":"response.content_part.added","item_id":"msg_a","content_index":0,"part":{"type":"output_text"}}`,
			`{"type":"response.output_text.delta","item_id":"msg_a","content_index":0,"delta":"Hel"}`,
			`{"type":"response.output_text.delta","item_id":"msg_a","content_index":0,"delta":"lo"}`,
			`{"type":"response.content_part.done","item_id":"msg_a","content_index":0}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_a","type":"message"}}`,
			completed), []string{start,
			`{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`,
			`{"delta":{"text":"Hel","type":"text_delta"},"index":0,"type":"content_block_delta"}`,
			`{"delta":{"text":"lo","type":"text_delta"},"index":0,"type":"content_block_delta"}`,
			`{"index":0,"type":"content_block_stop"}`,
			`{"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"a function call is a tool use", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\":"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"ls\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"command\":\"ls\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash","arguments":"{\"command\":\"ls\"}"}}`,
			completed), []string{start,
			`{"content_block":{"id":"call_1","input":{},"name":"Bash","type":"tool_use"},"index":0,"type":"content_block_start"}`,
			`{"delta":{"partial_json":"{\"command\":","type":"input_json_delta"},"index":0,"type":"content_block_delta"}`,
			`{"delta":{"partial_json":"\"ls\"}","type":"input_json_delta"},"index":0,"type":"content_block_delta"}`,
			`{"index":0,"type":"content_block_stop"}`,
			`{"delta":{"stop_reason":"tool_use","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"a function call whose arguments came whole", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash","arguments":"{}"}}`,
			completed), []string{start,
			`{"content_block":{"id":"call_1","input":{},"name":"Bash","type":"tool_use"},"index":0,"type":"content_block_start"}`,
			`{"delta":{"partial_json":"{}","type":"input_json_delta"},"index":0,"type":"content_block_delta"}`,
			`{"index":0,"type":"content_block_stop"}`,
			`{"delta":{"stop_reason":"tool_use","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"reasoning is thinking signed with its encrypted content, then text", shape{model: "gpt-6-luna", stream: true, think: true}, sse(created,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`,
			`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":0}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":0,"delta":"plan"}`,
			`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":1}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":1,"delta":"act"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1"}}`,
			`{"type":"response.output_text.delta","item_id":"msg_a","content_index":0,"delta":"ok"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_a","type":"message"}}`,
			completed), []string{start,
			`{"content_block":{"signature":"","thinking":"","type":"thinking"},"index":0,"type":"content_block_start"}`,
			`{"delta":{"thinking":"plan","type":"thinking_delta"},"index":0,"type":"content_block_delta"}`,
			`{"delta":{"thinking":"\n\n","type":"thinking_delta"},"index":0,"type":"content_block_delta"}`,
			`{"delta":{"thinking":"act","type":"thinking_delta"},"index":0,"type":"content_block_delta"}`,
			`{"delta":{"signature":"enc-1","type":"signature_delta"},"index":0,"type":"content_block_delta"}`,
			`{"index":0,"type":"content_block_stop"}`,
			`{"content_block":{"text":"","type":"text"},"index":1,"type":"content_block_start"}`,
			`{"delta":{"text":"ok","type":"text_delta"},"index":1,"type":"content_block_delta"}`,
			`{"index":1,"type":"content_block_stop"}`,
			`{"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"reasoning nobody asked to think is left out", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"plan"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1"}}`,
			completed), []string{start,
			`{"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"a web search is Anthropic's server tool use and its result", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`,
			`{"type":"response.web_search_call.searching","item_id":"ws_1"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"python latest","sources":[{"type":"url","url":"https://python.org/a"},{"type":"url","url":"https://python.org/b"}]}}}`,
			`{"type":"response.output_text.delta","item_id":"msg_a","content_index":0,"delta":"3.15"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_a","type":"message"}}`,
			completed), []string{start,
			`{"content_block":{"id":"srvtoolu_ws_1","input":{},"name":"web_search","type":"server_tool_use"},"index":0,"type":"content_block_start"}`,
			`{"delta":{"partial_json":"{\"query\":\"python latest\"}","type":"input_json_delta"},"index":0,"type":"content_block_delta"}`,
			`{"index":0,"type":"content_block_stop"}`,
			`{"content_block":{"content":[{"encrypted_content":"","page_age":null,"title":"https://python.org/a","type":"web_search_result","url":"https://python.org/a"},{"encrypted_content":"","page_age":null,"title":"https://python.org/b","type":"web_search_result","url":"https://python.org/b"}],"tool_use_id":"srvtoolu_ws_1","type":"web_search_tool_result"},"index":1,"type":"content_block_start"}`,
			`{"index":1,"type":"content_block_stop"}`,
			`{"content_block":{"text":"","type":"text"},"index":2,"type":"content_block_start"}`,
			`{"delta":{"text":"3.15","type":"text_delta"},"index":2,"type":"content_block_delta"}`,
			`{"index":2,"type":"content_block_stop"}`,
			`{"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"an answer cut at its output limit stops at max tokens", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":50}}}`),
			[]string{start, `{"delta":{"stop_reason":"max_tokens","stop_sequence":null},"type":"message_delta",` + usage + `}`, stop}},
		{"a failed answer is an error", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"slow down"}}}`),
			[]string{start, `{"error":{"message":"slow down","type":"rate_limit_error"},"type":"error"}`}},
		{"an overflowed context is a prompt too long", shape{model: "gpt-6-luna", stream: true}, sse(
			`{"type":"error","code":"context_length_exceeded","message":"too many tokens"}`),
			[]string{`{"error":{"message":"prompt is too long: too many tokens","type":"invalid_request_error"},"type":"error"}`}},
		{"a stream cut before its end is an error", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.output_text.delta","item_id":"msg_a","content_index":0,"delta":"o"}`),
			[]string{start,
				`{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}`,
				`{"delta":{"text":"o","type":"text_delta"},"index":0,"type":"content_block_delta"}`,
				`{"index":0,"type":"content_block_stop"}`,
				`{"error":{"message":"the model's answer ended before it was complete","type":"api_error"},"type":"error"}`}},
		{"an answer completed without its usage is an error", shape{model: "gpt-6-luna", stream: true}, sse(created,
			`{"type":"response.completed","response":{"id":"resp_1"}}`),
			[]string{start, `{"error":{"message":"the model's answer stated no usage","type":"api_error"},"type":"error"}`}},
		{"an answer completed without its start is an error", shape{model: "gpt-6-luna", stream: true}, sse(completed),
			[]string{`{"error":{"message":"the model's answer stated no usage","type":"api_error"},"type":"error"}`}},
		{"an event that is no JSON ends the answer in an error", shape{model: "gpt-6-luna", stream: true}, sse(created) + "data: {not json\n\n" + sse(completed),
			[]string{start, `{"error":{"message":"the model's answer holds an event that could not be read","type":"api_error"},"type":"error"}`}},
	} {
		got := anthropicEvents(t, tc.stream, tc.how)
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, strings.Join(got, "\n     "), strings.Join(tc.want, "\n     "))
		}
	}
}

func TestAnAnswerNotStreamedIsTheWholeMessage(t *testing.T) {
	stream := sse(created,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"enc-1"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_a","content_index":0,"delta":"ok"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\":\"ls\"}"}`,
		`{"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","arguments":"{\"command\":\"ls\"}"}}`,
		completed)
	status, body := wholeMessage(strings.NewReader(stream), shape{model: "gpt-6-luna", think: true, display: "omitted"}, zap.NewNop())
	want := `{"content":[{"signature":"enc-1","thinking":"","type":"thinking"},{"text":"ok","type":"text"},{"id":"call_1","input":{"command":"ls"},"name":"Bash","type":"tool_use"}],` +
		`"id":"msg_resp_1","model":"gpt-6-luna","role":"assistant","stop_reason":"tool_use","stop_sequence":null,"type":"message",` +
		`"usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":600,"input_tokens":400,"output_tokens":50}}`
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("status %d, body\n %s\nwant\n %s", status, body, want)
	}
	status, body = wholeMessage(strings.NewReader(sse(created, `{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`)), shape{}, zap.NewNop())
	if status != http.StatusInternalServerError || string(body) != `{"error":{"message":"boom","type":"api_error"},"type":"error"}` {
		t.Fatalf("a failed answer: status %d, body %s", status, body)
	}
	status, body = wholeMessage(strings.NewReader(sse(completed)), shape{}, zap.NewNop())
	if status != http.StatusInternalServerError || string(body) != `{"error":{"message":"the model's answer stated no usage","type":"api_error"},"type":"error"}` {
		t.Fatalf("an answer that never started: status %d, body %s", status, body)
	}
}

func TestARefusalIsAnAnthropicError(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, want string
	}{
		{http.StatusBadRequest, `{"detail":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}`,
			`{"error":{"message":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"},"type":"error"}`},
		{http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":60}}`,
			`{"error":{"message":"The usage limit has been reached","type":"rate_limit_error"},"type":"error"}`},
		{http.StatusBadRequest, `{"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window"}}`,
			`{"error":{"message":"prompt is too long: Your input exceeds the context window","type":"invalid_request_error"},"type":"error"}`},
		{http.StatusBadGateway, ``, `{"error":{"message":"Bad Gateway","type":"api_error"},"type":"error"}`},
	} {
		if status, got, _ := refusedAnswer(tc.status, []byte(tc.body), time.Now()); status != tc.status || string(got) != tc.want {
			t.Errorf("%d %s: %d %s, want %s", tc.status, tc.body, status, got, tc.want)
		}
	}
	// A refusal of the platform's credential is never the caller's to sign in again for.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		got, body, _ := refusedAnswer(status, []byte(`{"error":"invalid token"}`), time.Now())
		if want := `{"error":{"message":"the ChatGPT backend refused the platform's credential: invalid token","type":"api_error"},"type":"error"}`; got != http.StatusBadGateway || string(body) != want {
			t.Errorf("%d: %d %s, want 502 %s", status, got, body, want)
		}
	}
}

// A ChatGPT subscription out of its usage says when it takes calls again, which reaches Claude Code
// as Anthropic's API says it of a Claude subscription.
func TestAUsageLimitSaysWhenTheSubscriptionTakesCallsAgain(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for body, want := range map[string]time.Time{
		`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":60}}`: now.Add(time.Minute),
		`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_at":1800003600}}`: time.Unix(1_800_003_600, 0),
		`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`:                        {},
		`{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached","resets_in_seconds":60}}`:               {},
	} {
		if _, _, got := refusedAnswer(http.StatusTooManyRequests, []byte(body), now); !got.Equal(want) {
			t.Errorf("%s: resets at %v, want %v", body, got, want)
		}
	}
	h := http.Header{}
	refusedUntil(h, now.Add(90*time.Second), now)
	if h.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" || h.Get("Anthropic-Ratelimit-Unified-Reset") != "1800000090" || h.Get("Retry-After") != "90" {
		t.Errorf("headers %v", h)
	}
}

const (
	chatGPTAccess  = "chatgpt-access-token-only-this-test-knows"
	chatGPTAccount = "account-1"
)

type chatGPTCredentials struct {
	err     error
	expired chan string
}

func (c chatGPTCredentials) Fresh(context.Context) (domain.ChatGPTCredential, error) {
	return domain.ChatGPTCredential{AccessToken: chatGPTAccess, AccountID: chatGPTAccount}, c.err
}

func (c chatGPTCredentials) Expire(_ context.Context, accessToken string) error {
	c.expired <- accessToken
	return nil
}

var openAICatalog = domain.ModelCatalog{"gpt-6-luna": {InputUSDPerMTok: 2, OutputUSDPerMTok: 10, CacheReadUSDPerMTok: 0.5}}

func callOpenAI(t *testing.T, api, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, api+Prefix+"openai/v1/messages?beta=true", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	agent := token(t, scope.Agent("pe-1", "agent-oai"))
	req.Header.Set("Authorization", "Bearer "+agent)
	req.Header.Set("X-Api-Key", agent)
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("X-Claude-Code-Session-Id", "session-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// openAIAPI is the API with the openai provider at a fake ChatGPT backend that answers with
// provider, and what the backend received.
func openAIAPI(t *testing.T, credentials ChatGPTCredentials, provider http.HandlerFunc) (*fixture, string) {
	t.Helper()
	f := newFixture(t, provider)
	backend := f.upstream
	f.api = newAPI(t, Providers{OpenAI: &OpenAI{URL: backend + "/backend-api/codex/responses", Credentials: credentials}}, f.usage, budgets{}, 8)
	return f, backend
}

const toolCall = `{"model":"gpt-6-luna","stream":true,"max_tokens":32000,"system":[{"type":"text","text":"be brief"}],
"messages":[{"role":"user","content":"list the files"}],
"tools":[{"name":"Bash","description":"runs a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"low"}}`

func TestAnOpenAICallReachesTheChatGPTBackendAsTheCodexCLIMakesItAndIsMeteredAtTheCatalog(t *testing.T) {
	f, _ := openAIAPI(t, chatGPTCredentials{}, streamed(sse(created,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\":\"ls\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","arguments":"{\"command\":\"ls\"}"}}`,
		completed)))
	resp := callOpenAI(t, f.api.URL, toolCall)
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), answer)
	}
	for _, want := range []string{"event: message_start", `"type":"tool_use"`, `"partial_json":"{\"command\":\"ls\"}"`, `"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(string(answer), want) {
			t.Errorf("the answer holds no %s:\n%s", want, answer)
		}
	}
	got := f.received[0]
	if got.URL.Path != "/backend-api/codex/responses" || got.Method != http.MethodPost {
		t.Errorf("called %s %s, want POST /backend-api/codex/responses", got.Method, got.URL.Path)
	}
	for name, want := range map[string]string{"Authorization": "Bearer " + chatGPTAccess, "Chatgpt-Account-Id": chatGPTAccount,
		"Openai-Beta": "responses=experimental", "Originator": "codex_cli_rs", "Accept": "text/event-stream",
		"X-Api-Key": "", "Anthropic-Beta": "", "X-Claude-Code-Session-Id": "", "Cookie": ""} {
		if have := got.Header.Get(name); have != want {
			t.Errorf("header %s = %q, want %q", name, have, want)
		}
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body["instructions"] != "be brief" || body["store"] != false || body["stream"] != true || body["model"] != "gpt-6-luna" || body["max_tokens"] != nil {
		t.Errorf("body = %s", f.bodies[0])
	}
	call := f.usage.wait(t)
	if call.Provider != domain.ModelProviderOpenAI || call.Model != "gpt-6-luna" || call.InputTokens != 400 || call.CacheReadInputTokens != 600 || call.OutputTokens != 50 {
		t.Errorf("recorded %+v", call)
	}
	if want := (400*2 + 600*0.5 + 50*10) / 1e6; call.CostUSD != want {
		t.Errorf("cost = %v, want %v", call.CostUSD, want)
	}
}

// An answer that failed after it spent tokens is metered on what it states it spent, streamed or
// not.
func TestAFailedOpenAIAnswerIsMeteredOnWhatItSpent(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp_1","error":{"code":"server_error","message":"boom"},"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":50}}}`
	for name, body := range map[string]string{"streamed": toolCall, "whole": strings.Replace(toolCall, `"stream":true`, `"stream":false`, 1)} {
		t.Run(name, func(t *testing.T) {
			f, _ := openAIAPI(t, chatGPTCredentials{}, streamed(sse(created,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Bash"}}`, failed)))
			resp := callOpenAI(t, f.api.URL, body)
			answer, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !strings.Contains(string(answer), `"message":"boom"`) {
				t.Fatalf("status %d: %s, want the error", resp.StatusCode, answer)
			}
			call := f.usage.wait(t)
			if call.InputTokens != 400 || call.CacheReadInputTokens != 600 || call.OutputTokens != 50 {
				t.Errorf("recorded %+v", call)
			}
		})
	}
}

func TestAnOpenAIRefusalReachesTheAgentAsAnAnthropicErrorAndIsNotMetered(t *testing.T) {
	f, _ := openAIAPI(t, chatGPTCredentials{}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
	})
	resp := callOpenAI(t, f.api.URL, toolCall)
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || string(answer) != `{"error":{"message":"The usage limit has been reached","type":"rate_limit_error"},"type":"error"}` {
		t.Fatalf("status %d: %s", resp.StatusCode, answer)
	}
	reset, err := strconv.ParseInt(resp.Header.Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64)
	if resp.Header.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" || err != nil || time.Until(time.Unix(reset, 0)) < 59*time.Minute || resp.Header.Get("Retry-After") == "" {
		t.Errorf("headers %v, want the subscription refused for the hour it said", resp.Header)
	}
	f.usage.none(t)
}

// The ChatGPT backend refusing the platform's access token is a 502 api_error, so Claude Code does
// not sign out, and makes that credential due: the next call renews it.
func TestAnOpenAICredentialTheBackendRefusedIsABadGatewayAndIsRenewedNext(t *testing.T) {
	credentials := chatGPTCredentials{expired: make(chan string, 1)}
	f, _ := openAIAPI(t, credentials, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"Could not parse your authentication token."}`)
	})
	resp := callOpenAI(t, f.api.URL, toolCall)
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(answer), `"type":"api_error"`) {
		t.Fatalf("status %d: %s, want 502 api_error", resp.StatusCode, answer)
	}
	select {
	case expired := <-credentials.expired:
		if expired != chatGPTAccess {
			t.Errorf("expired %q, want the access token the call was made with", expired)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the refused credential was not made due")
	}
	f.usage.none(t)
}

// The ChatGPT backend states a call's usage only at the end of its answer: an agent that hangs up
// leaves the call to run to its end, and its whole usage is recorded at the catalog's price.
func TestAnOpenAICallTheAgentHungUpOnIsReadToItsEndForItsUsage(t *testing.T) {
	hungUp := make(chan struct{})
	f, _ := openAIAPI(t, chatGPTCredentials{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sse(created, `{"type":"response.content_part.added","item_id":"msg_1","content_index":0}`, `{"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"hi"}`))
		w.(http.Flusher).Flush()
		<-hungUp
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, sse(completed))
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.api.URL+Prefix+"openai/v1/messages", strings.NewReader(toolCall))
	req.Header.Set("Authorization", "Bearer "+token(t, scope.Agent("pe-1", "agent-oai")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	close(hungUp)
	call := f.usage.wait(t)
	if call.AgentID != "agent-oai" || call.InputTokens != 400 || call.CacheReadInputTokens != 600 || call.OutputTokens != 50 || call.CostUSD != (400*2+600*0.5+50*10)/1e6 {
		t.Fatalf("recorded %+v, want the call's final usage and cost", call)
	}
}

func TestAnOpenAICallTheProxyCannotMakeIsAnAnthropicError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		credentials ChatGPTCredentials
		body        string
		status      int
		kind        string
	}{
		{"a call without a counterpart", chatGPTCredentials{}, `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"code_execution_20250825","name":"code_execution"}]}`, http.StatusBadRequest, "invalid_request_error"},
		{"a credential that cannot be renewed", chatGPTCredentials{err: errors.New("refused")}, toolCall, http.StatusBadGateway, "api_error"},
	} {
		f, _ := openAIAPI(t, tc.credentials, streamed(sse(created, completed)))
		resp := callOpenAI(t, f.api.URL, tc.body)
		var answer struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&answer)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || answer.Type != "error" || answer.Error.Type != tc.kind || f.upstreamCalls() != 0 {
			t.Errorf("%s: status %d, %+v after %d calls, want %d %s before any", tc.name, resp.StatusCode, answer, f.upstreamCalls(), tc.status, tc.kind)
		}
	}
}

func TestAnOpenAIModelOutsideTheCatalogAndCountingTokensAreRefused(t *testing.T) {
	f, _ := openAIAPI(t, chatGPTCredentials{}, streamed(sse(created, completed)))
	if status, reason, _ := refused(t, callOpenAI(t, f.api.URL, `{"model":"gpt-5.4","messages":[]}`)); status != http.StatusForbidden || reason != domain.ReasonModelNotTheCallers {
		t.Errorf("another model than the agent's: %d %s, want 403 %s", status, reason, domain.ReasonModelNotTheCallers)
	}
	req, _ := http.NewRequest(http.MethodPost, f.api.URL+Prefix+"openai/v1/messages/count_tokens", strings.NewReader(`{"model":"gpt-6-luna","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+token(t, scope.Agent("pe-1", "agent-oai")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || f.upstreamCalls() != 0 {
		t.Errorf("counting tokens: %d after %d calls, want 404 before any", resp.StatusCode, f.upstreamCalls())
	}
}

// An answer the ChatGPT backend completed without its usage reaches the agent as an error, and no
// call is recorded at the zeros of its start.
func TestAnOpenAIAnswerCompletedWithoutItsUsageIsAnErrorAndNotRecorded(t *testing.T) {
	f, _ := openAIAPI(t, chatGPTCredentials{}, streamed(sse(created, `{"type":"response.completed","response":{"id":"resp_1"}}`)))
	resp := callOpenAI(t, f.api.URL, toolCall)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `"message":"the model's answer stated no usage"`) {
		t.Fatalf("answer %s, want an error event", body)
	}
	f.usage.none(t)
}
