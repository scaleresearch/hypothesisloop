package openrouter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

const listing = `{"data":[
 {"id":"moonshotai/kimi-k2","name":"MoonshotAI: Kimi K2","context_length":131072,"pricing":{"prompt":"0.0000006","completion":"0.0000025","input_cache_read":"0.00000015"},"supported_parameters":["tools"]},
 {"id":"openai/gpt-5-nano","name":"OpenAI: GPT-5 Nano","context_length":400000,"pricing":{"prompt":"0.00000005","completion":"0.0000004"}},
 {"id":"openrouter/auto","name":"Auto Router","context_length":2000000,"pricing":{"prompt":"-1","completion":"-1"}}
]}`

// fake is OpenRouter's API, answering each listing with answer, and the requests it got.
func fake(t *testing.T, status int, answer string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var got []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return server, &got
}

// A model whose price OpenRouter states as -1, known only once a call is made, is not listed.
func TestTheModelsThatTakeToolsAreListedWithTheirPricePerMillionTokens(t *testing.T) {
	server, got := fake(t, http.StatusOK, listing)
	models, err := New(server.URL).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "moonshotai/kimi-k2", Name: "MoonshotAI: Kimi K2", ContextLength: 131072, InputUSDPerMTok: 0.6, OutputUSDPerMTok: 2.5},
		{ID: "openai/gpt-5-nano", Name: "OpenAI: GPT-5 Nano", ContextLength: 400000, InputUSDPerMTok: 0.05, OutputUSDPerMTok: 0.4},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	if r := (*got)[0]; r.URL.Path != "/v1/models" || r.URL.Query().Get("supported_parameters") != "tools" || r.Header.Get("Authorization") != "" {
		t.Fatalf("asked %s?%s with Authorization %q, want the public listing of models that take tools", r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
	}
}

// The listing is read again once it is older than cacheFor, and not before.
func TestAListingIsAnsweredAgainForAFewMinutesOnly(t *testing.T) {
	server, got := fake(t, http.StatusOK, listing)
	c := New(server.URL)
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }
	for _, after := range []time.Duration{0, time.Minute, cacheFor - time.Second} {
		now = time.Unix(1_800_000_000, 0).Add(after)
		if _, err := c.Models(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(*got) != 1 {
		t.Fatalf("read %d times within %s, want once", len(*got), cacheFor)
	}
	now = now.Add(time.Second)
	if _, err := c.Models(context.Background()); err != nil || len(*got) != 2 {
		t.Fatalf("after %s: read %d times, %v, want a second read", cacheFor, len(*got), err)
	}
}

func TestAListingOpenRouterDoesNotAnswerIsAnError(t *testing.T) {
	for name, answer := range map[string]string{
		"a failure":            "",
		"no JSON":              "not json",
		"a price of no number": `{"data":[{"id":"a/b","pricing":{"prompt":"free","completion":"0"}}]}`,
	} {
		status := http.StatusOK
		if answer == "" {
			status = http.StatusBadGateway
		}
		api, _ := fake(t, status, answer)
		if models, err := New(api.URL).Models(context.Background()); err == nil {
			t.Errorf("%s: models = %+v, want an error", name, models)
		}
	}
}
