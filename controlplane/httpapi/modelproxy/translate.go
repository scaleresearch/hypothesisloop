package modelproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// An openai call is made in Anthropic's Messages shape, as Claude Code makes every call, and
// passed on in OpenAI's Responses shape, as the Codex CLI makes one on a ChatGPT subscription;
// the answer comes back the other way. A request field neither shape shares, such as max_tokens,
// temperature, stop_sequences or cache_control, which the ChatGPT backend refuses or has no
// counterpart of, is not passed on.

// messagesRequest is the part of an Anthropic Messages call that is translated.
type messagesRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		// The settings of the web_search server tool.
		AllowedDomains []string        `json:"allowed_domains"`
		BlockedDomains []string        `json:"blocked_domains"`
		UserLocation   json.RawMessage `json:"user_location"`
	} `json:"tools"`
	ToolChoice *struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Thinking *struct {
		Type    string `json:"type"`
		Display string `json:"display"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Stream bool `json:"stream"`
}

// block is one content block of an Anthropic message.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

// responsesRequest is a call to the ChatGPT backend's Responses endpoint.
type responsesRequest struct {
	Model             string     `json:"model"`
	Instructions      string     `json:"instructions"`
	Input             []any      `json:"input"`
	Tools             []any      `json:"tools"`
	ToolChoice        any        `json:"tool_choice"`
	ParallelToolCalls bool       `json:"parallel_tool_calls"`
	Reasoning         *reasoning `json:"reasoning,omitempty"`
	Include           []string   `json:"include"`
	Store             bool       `json:"store"`
	Stream            bool       `json:"stream"`
	PromptCacheKey    string     `json:"prompt_cache_key,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []part `json:"content"`
}

type part struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type reasoningItem struct {
	Type             string `json:"type"`
	Summary          []part `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
}

// webSearchTool is the Responses API's own web search, Anthropic's web_search server tool.
type webSearchTool struct {
	Type         string          `json:"type"`
	Filters      *searchFilters  `json:"filters,omitempty"`
	UserLocation json.RawMessage `json:"user_location,omitempty"`
}

type searchFilters struct {
	AllowedDomains []string `json:"allowed_domains"`
}

type functionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

// efforts is OpenAI's reasoning effort for each of Anthropic's.
var efforts = map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}

// shape is what the translated answer is asked for in: streamed or whole, and with what it
// thought, its summary unless display is omitted.
type shape struct {
	model, display string
	stream, think  bool
}

// toResponses is the Responses call of an Anthropic Messages call on modelID. cacheKey groups the
// calls of one session, whose prompts share their start.
func toResponses(body []byte, modelID, cacheKey string) ([]byte, shape, error) {
	var in messagesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, shape{}, fmt.Errorf("the call is no Messages request: %w", err)
	}
	out := responsesRequest{Model: modelID, Input: []any{}, Tools: []any{}, ToolChoice: "auto", ParallelToolCalls: true,
		Include: []string{}, Stream: true, PromptCacheKey: cacheKey}
	how := shape{model: in.Model, stream: in.Stream}
	instructions, err := systemText(in.System)
	if err != nil {
		return nil, shape{}, err
	}
	out.Instructions = instructions
	searchTool := ""
	for _, t := range in.Tools {
		switch {
		case t.Type == "" || t.Type == "custom":
			out.Tools = append(out.Tools, functionTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
		case strings.HasPrefix(t.Type, "web_search_"):
			// max_uses has no counterpart: the model searches as often as it needs.
			if len(t.BlockedDomains) > 0 {
				return nil, shape{}, fmt.Errorf("tool %s blocks domains, which OpenAI's web search cannot", t.Name)
			}
			search := webSearchTool{Type: "web_search", UserLocation: t.UserLocation}
			if len(t.AllowedDomains) > 0 {
				search.Filters = &searchFilters{AllowedDomains: t.AllowedDomains}
			}
			out.Tools = append(out.Tools, search)
			out.Include = append(out.Include, "web_search_call.action.sources")
			searchTool = t.Name
		default:
			return nil, shape{}, fmt.Errorf("tool %s is Anthropic's server tool %s, which an openai model has not", t.Name, t.Type)
		}
	}
	if c := in.ToolChoice; c != nil {
		switch c.Type {
		case "auto":
		case "any":
			out.ToolChoice = "required"
		case "none":
			out.ToolChoice = "none"
		case "tool":
			out.ToolChoice = map[string]string{"type": "function", "name": c.Name}
			if c.Name == searchTool {
				out.ToolChoice = map[string]string{"type": "web_search"}
			}
		default:
			return nil, shape{}, fmt.Errorf("tool_choice %q is none of auto, any, tool and none", c.Type)
		}
		out.ParallelToolCalls = !c.DisableParallelToolUse
	}
	if in.Thinking != nil && (in.Thinking.Type == "enabled" || in.Thinking.Type == "adaptive") {
		how.think, how.display = true, in.Thinking.Display
		out.Reasoning = &reasoning{}
		if how.display != "omitted" {
			out.Reasoning.Summary = "auto"
		}
		out.Include = append(out.Include, "reasoning.encrypted_content")
	}
	if in.OutputConfig != nil && in.OutputConfig.Effort != "" {
		effort, ok := efforts[in.OutputConfig.Effort]
		if !ok {
			return nil, shape{}, fmt.Errorf("effort %q is none of low, medium, high, xhigh and max", in.OutputConfig.Effort)
		}
		if out.Reasoning == nil {
			out.Reasoning = &reasoning{}
		}
		out.Reasoning.Effort = effort
	}
	for i, m := range in.Messages {
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, shape{}, fmt.Errorf("message %d: %w", i, err)
		}
		items, err := inputItems(m.Role, blocks)
		if err != nil {
			return nil, shape{}, fmt.Errorf("message %d: %w", i, err)
		}
		out.Input = append(out.Input, items...)
	}
	translated, err := json.Marshal(out)
	return translated, how, err
}

// systemText is the system prompt, a string or text blocks, as one text.
func systemText(raw json.RawMessage) (string, error) {
	blocks, err := contentBlocks(raw)
	if err != nil {
		return "", fmt.Errorf("system: %w", err)
	}
	texts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("system: a %s block is no text", b.Type)
		}
		texts = append(texts, b.Text)
	}
	return strings.Join(texts, "\n\n"), nil
}

// contentBlocks is content that is a string, one text block, or blocks.
func contentBlocks(raw json.RawMessage) ([]block, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("content is neither a string nor content blocks")
	}
	return blocks, nil
}

// inputItems is the Responses input of one message, in its order: a user's text, images and
// documents, and its tool results, an assistant's thinking, text and tool calls, and the text of
// a system message within the conversation, the developer's in OpenAI's words.
func inputItems(role string, blocks []block) ([]any, error) {
	var items []any
	var pending []part
	itemRole := map[string]string{"user": "user", "assistant": "assistant", "system": "developer"}[role]
	if itemRole == "" {
		return nil, fmt.Errorf("role %q is none of user, assistant and system", role)
	}
	flush := func() {
		if len(pending) > 0 {
			items = append(items, messageItem{Type: "message", Role: itemRole, Content: pending})
			pending = nil
		}
	}
	textType := map[string]string{"user": "input_text", "assistant": "output_text", "system": "input_text"}[role]
	for _, b := range blocks {
		switch {
		case b.Type == "text":
			pending = append(pending, part{Type: textType, Text: b.Text})
		case role == "user" && (b.Type == "image" || b.Type == "document"):
			p, err := mediaPart(b)
			if err != nil {
				return nil, err
			}
			pending = append(pending, p)
		case role == "user" && b.Type == "tool_result":
			flush()
			output, images, err := toolOutput(b)
			if err != nil {
				return nil, err
			}
			items = append(items, functionCallOutputItem{Type: "function_call_output", CallID: b.ToolUseID, Output: output})
			// A function's output is text: what it showed follows it, as the user's.
			if len(images) > 0 {
				items = append(items, messageItem{Type: "message", Role: "user", Content: images})
			}
		case role == "assistant" && b.Type == "tool_use":
			flush()
			arguments := string(b.Input)
			if len(bytes.TrimSpace(b.Input)) == 0 {
				arguments = "{}"
			}
			items = append(items, functionCallItem{Type: "function_call", CallID: b.ID, Name: b.Name, Arguments: arguments})
		case role == "assistant" && b.Type == "thinking":
			flush()
			// What an openai model thought is carried in its encrypted content, the block's
			// signature; a block without one carries nothing the model can read again.
			if b.Signature == "" {
				continue
			}
			summary := []part{}
			if b.Thinking != "" {
				summary = append(summary, part{Type: "summary_text", Text: b.Thinking})
			}
			items = append(items, reasoningItem{Type: "reasoning", Summary: summary, EncryptedContent: b.Signature})
		default:
			return nil, fmt.Errorf("a %s block of the %s has no counterpart for an openai model", b.Type, role)
		}
	}
	flush()
	return items, nil
}

// mediaPart is an image or a document as the user's input.
func mediaPart(b block) (part, error) {
	s := b.Source
	if s == nil {
		return part{}, fmt.Errorf("the %s block has no source", b.Type)
	}
	switch {
	case s.Type == "base64" && b.Type == "image":
		return part{Type: "input_image", ImageURL: "data:" + s.MediaType + ";base64," + s.Data}, nil
	case s.Type == "url" && b.Type == "image":
		return part{Type: "input_image", ImageURL: s.URL}, nil
	case s.Type == "base64" && b.Type == "document":
		return part{Type: "input_file", Filename: "document.pdf", FileData: "data:" + s.MediaType + ";base64," + s.Data}, nil
	case s.Type == "text" && b.Type == "document":
		return part{Type: "input_text", Text: s.Data}, nil
	}
	return part{}, fmt.Errorf("a %s of source %s has no counterpart for an openai model", b.Type, s.Type)
}

// toolOutput is a tool result's text and the images it holds.
func toolOutput(b block) (string, []part, error) {
	blocks, err := contentBlocks(b.Content)
	if err != nil {
		return "", nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
	}
	var texts []string
	var images []part
	for _, c := range blocks {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image", "document":
			p, err := mediaPart(c)
			if err != nil {
				return "", nil, err
			}
			images = append(images, p)
		default:
			return "", nil, fmt.Errorf("tool_result %s: a %s block has no counterpart for an openai model", b.ToolUseID, c.Type)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

// event is one event of a Responses stream, the fields of every kind translated.
type event struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index"`
	SummaryIndex int    `json:"summary_index"`
	Delta        string `json:"delta"`
	Code         string `json:"code"`
	Message      string `json:"message"`
	Item         struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
		Action           *struct {
			Type    string `json:"type"`
			Query   string `json:"query"`
			Sources []struct {
				URL string `json:"url"`
			} `json:"sources"`
		} `json:"action"`
	} `json:"item"`
	Response struct {
		ID                string `json:"id"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// sink takes the Anthropic events of an answer, each its type and its data.
type sink func(kind string, data map[string]any)

// translator turns the events of a Responses stream into those of an Anthropic Messages stream.
// One content block is open at a time, as the Responses stream sends one item at a time.
type translator struct {
	how       shape
	emit      sink
	started   bool
	finished  bool
	index     int
	open      string
	sentInput bool
	toolUsed  bool
}

func (t *translator) start(id string) {
	t.started = true
	t.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + id, "type": "message", "role": "assistant", "model": t.how.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (t *translator) openBlock(key string, content map[string]any) {
	t.close()
	t.open, t.sentInput = key, false
	t.emit("content_block_start", map[string]any{"type": "content_block_start", "index": t.index, "content_block": content})
}

func (t *translator) delta(delta map[string]any) {
	t.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": t.index, "delta": delta})
}

func (t *translator) close() {
	if t.open == "" {
		return
	}
	t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.index})
	t.open = ""
	t.index++
}

// fail ends the answer with an error event.
func (t *translator) fail(kind, message string) {
	t.close()
	t.finished = true
	t.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
}

// take translates one event of the stream.
func (t *translator) take(e event) {
	if t.finished {
		return
	}
	textKey := e.ItemID + ":" + fmt.Sprint(e.ContentIndex)
	switch e.Type {
	case "response.created":
		t.start(e.Response.ID)
	case "response.output_item.added":
		switch e.Item.Type {
		case "function_call":
			t.toolUsed = true
			t.openBlock(e.Item.ID, map[string]any{"type": "tool_use", "id": e.Item.CallID, "name": e.Item.Name, "input": map[string]any{}})
		case "reasoning":
			if t.how.think {
				t.openBlock(e.Item.ID, map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
		}
	case "response.content_part.added":
		t.openBlock(textKey, map[string]any{"type": "text", "text": ""})
	case "response.output_text.delta", "response.refusal.delta":
		if t.open != textKey {
			t.openBlock(textKey, map[string]any{"type": "text", "text": ""})
		}
		t.delta(map[string]any{"type": "text_delta", "text": e.Delta})
	case "response.content_part.done":
		if t.open == textKey {
			t.close()
		}
	case "response.function_call_arguments.delta":
		if t.open == e.ItemID && e.Delta != "" {
			t.sentInput = true
			t.delta(map[string]any{"type": "input_json_delta", "partial_json": e.Delta})
		}
	case "response.reasoning_summary_part.added":
		if t.open == e.ItemID && e.SummaryIndex > 0 && t.how.display != "omitted" {
			t.delta(map[string]any{"type": "thinking_delta", "thinking": "\n\n"})
		}
	case "response.reasoning_summary_text.delta":
		if t.open == e.ItemID && t.how.display != "omitted" {
			t.delta(map[string]any{"type": "thinking_delta", "thinking": e.Delta})
		}
	case "response.output_item.done":
		if e.Item.Type == "web_search_call" {
			t.searched(e)
			return
		}
		if t.open == e.Item.ID {
			switch e.Item.Type {
			case "function_call":
				if !t.sentInput && e.Item.Arguments != "" {
					t.delta(map[string]any{"type": "input_json_delta", "partial_json": e.Item.Arguments})
				}
			case "reasoning":
				t.delta(map[string]any{"type": "signature_delta", "signature": e.Item.EncryptedContent})
			}
			t.close()
		} else if strings.HasPrefix(t.open, e.Item.ID+":") {
			t.close()
		}
	case "response.completed", "response.incomplete":
		t.complete(e)
	case "response.failed":
		code, message := "", "the model call failed"
		if failure := e.Response.Error; failure != nil {
			code, message = failure.Code, failure.Message
		}
		t.fail(errorType(code, http.StatusInternalServerError), errorMessage(code, message))
	case "error":
		code, message := e.Code, e.Message
		if e.Error != nil {
			code, message = e.Error.Code, e.Error.Message
		}
		t.fail(errorType(code, http.StatusInternalServerError), errorMessage(code, message))
	}
}

// searched is a web search the model made, as Anthropic answers one of its web_search server
// tool: the search, then what it found. OpenAI states each source by its address alone.
func (t *translator) searched(e event) {
	id := "srvtoolu_" + e.Item.ID
	query, results := "", []map[string]any{}
	if a := e.Item.Action; a != nil {
		query = a.Query
		for _, s := range a.Sources {
			results = append(results, map[string]any{"type": "web_search_result", "url": s.URL, "title": s.URL, "encrypted_content": "", "page_age": nil})
		}
	}
	input, _ := json.Marshal(map[string]string{"query": query})
	t.openBlock(e.Item.ID, map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{}})
	t.delta(map[string]any{"type": "input_json_delta", "partial_json": string(input)})
	t.openBlock(e.Item.ID+":result", map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": results})
	t.close()
}

// complete ends the answer with its stop reason and its usage. OpenAI counts the cached input
// within the input, Anthropic beside it.
func (t *translator) complete(e event) {
	t.close()
	t.finished = true
	stop := "end_turn"
	if t.toolUsed {
		stop = "tool_use"
	}
	if d := e.Response.IncompleteDetails; e.Type == "response.incomplete" && d != nil {
		switch d.Reason {
		case "max_output_tokens":
			stop = "max_tokens"
		case "content_filter":
			stop = "refusal"
		}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
	if u := e.Response.Usage; u != nil {
		cached := u.InputTokensDetails.CachedTokens
		usage = map[string]any{"input_tokens": u.InputTokens - cached, "output_tokens": u.OutputTokens,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": cached}
	}
	t.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": usage})
	t.emit("message_stop", map[string]any{"type": "message_stop"})
}

// end ends a stream that stopped: one that stopped before its answer was complete ends in an error.
func (t *translator) end(err error) {
	if t.finished {
		return
	}
	message := "the model's answer ended before it was complete"
	if err != nil {
		message += ": " + err.Error()
	}
	t.fail("api_error", message)
}

// errorType is Anthropic's error type for OpenAI's code of an error, or for its HTTP status.
func errorType(code string, status int) string {
	switch code {
	case "rate_limit_exceeded", "usage_limit_reached":
		return "rate_limit_error"
	case "usage_not_included", "insufficient_quota":
		return "permission_error"
	case "context_length_exceeded", "invalid_prompt", "invalid_request_error":
		return "invalid_request_error"
	case "server_is_overloaded", "server_overloaded", "slow_down":
		return "overloaded_error"
	}
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	return "api_error"
}

// errorMessage is the message of an error, a context that overflowed said as Anthropic says it,
// which Claude Code compacts its context on.
func errorMessage(code, message string) string {
	if code == "context_length_exceeded" {
		return "prompt is too long: " + message
	}
	return message
}

// errorBody is an Anthropic error answer.
func errorBody(kind, message string) []byte {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
	return body
}

// refusedAnswer is the status and the Anthropic error answer of a call the ChatGPT backend refused
// with status and body, whose message it states as {"detail": ...} or {"error": ...}. A refusal of
// the platform's credential, 401 or 403, is the platform's failure and none of the caller's: it is
// a 502 api_error, where an authentication_error would have Claude Code sign out.
func refusedAnswer(status int, body []byte) (int, []byte) {
	var refused struct {
		Detail any             `json:"detail"`
		Error  json.RawMessage `json:"error"`
	}
	code, message := "", strings.TrimSpace(string(body))
	if json.Unmarshal(body, &refused) == nil {
		var detailed struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		var plain string
		switch {
		case json.Unmarshal(refused.Error, &detailed) == nil && detailed.Message != "":
			code, message = detailed.Code, detailed.Message
			if code == "" {
				code = detailed.Type
			}
		case json.Unmarshal(refused.Error, &plain) == nil && plain != "":
			message = plain
		case refused.Detail != nil:
			message = fmt.Sprint(refused.Detail)
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return http.StatusBadGateway, errorBody("api_error", "the ChatGPT backend refused the platform's credential: "+message)
	}
	return status, errorBody(errorType(code, status), errorMessage(code, message))
}

// readEvents hands each event of the Responses stream r to t, and ends t when r ends.
func readEvents(r *bufio.Reader, t *translator) error {
	line, err := r.ReadBytes('\n')
	if data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:")); ok {
		var e event
		if json.Unmarshal(bytes.TrimSpace(data), &e) == nil {
			t.take(e)
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = nil
		}
		t.end(err)
		return io.EOF
	}
	return nil
}

// translatedStream is the Anthropic stream of a Responses stream, translated as it is read.
type translatedStream struct {
	upstream io.ReadCloser
	lines    *bufio.Reader
	t        *translator
	out      bytes.Buffer
	ended    bool
}

func newTranslatedStream(upstream io.ReadCloser, how shape) *translatedStream {
	s := &translatedStream{upstream: upstream, lines: bufio.NewReader(upstream)}
	s.t = &translator{how: how, emit: func(kind string, data map[string]any) {
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(&s.out, "event: %s\ndata: %s\n\n", kind, encoded)
	}}
	return s
}

func (s *translatedStream) Read(b []byte) (int, error) {
	for s.out.Len() == 0 && !s.ended {
		if readEvents(s.lines, s.t) != nil {
			s.ended = true
		}
	}
	if s.out.Len() == 0 {
		return 0, io.EOF
	}
	return s.out.Read(b)
}

func (s *translatedStream) Close() error { return s.upstream.Close() }

// wholeMessage is the Anthropic message of a Responses stream read to its end, or the error it
// ended in with that error's HTTP status.
func wholeMessage(upstream io.Reader, how shape) (int, []byte) {
	message := map[string]any{}
	var content []map[string]any
	var partial []string
	var failure map[string]any
	t := &translator{how: how, emit: func(kind string, data map[string]any) {
		switch kind {
		case "message_start":
			message = data["message"].(map[string]any)
		case "content_block_start":
			content = append(content, data["content_block"].(map[string]any))
			partial = append(partial, "")
		case "content_block_delta":
			b, d, i := content[len(content)-1], data["delta"].(map[string]any), len(content)-1
			switch d["type"] {
			case "text_delta":
				b["text"] = b["text"].(string) + d["text"].(string)
			case "thinking_delta":
				b["thinking"] = b["thinking"].(string) + d["thinking"].(string)
			case "signature_delta":
				b["signature"] = d["signature"]
			case "input_json_delta":
				partial[i] += d["partial_json"].(string)
			}
		case "content_block_stop":
			if i := len(content) - 1; (content[i]["type"] == "tool_use" || content[i]["type"] == "server_tool_use") && partial[i] != "" {
				content[i]["input"] = json.RawMessage(partial[i])
			}
		case "message_delta":
			message["stop_reason"] = data["delta"].(map[string]any)["stop_reason"]
			message["usage"] = data["usage"]
		case "error":
			failure = data["error"].(map[string]any)
		}
	}}
	lines := bufio.NewReader(upstream)
	for readEvents(lines, t) == nil {
	}
	if failure != nil {
		kind, _ := failure["type"].(string)
		return errorStatus(kind), errorBody(kind, failure["message"].(string))
	}
	if content == nil {
		content = []map[string]any{}
	}
	message["content"] = content
	body, _ := json.Marshal(message)
	return http.StatusOK, body
}

// errorStatus is the HTTP status Anthropic answers an error of kind with.
func errorStatus(kind string) int {
	switch kind {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "permission_error":
		return http.StatusForbidden
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return 529
	}
	return http.StatusInternalServerError
}
