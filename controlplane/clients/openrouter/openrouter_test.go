package openrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

const listing = `{"data":[
 {"id":"moonshotai/kimi-k2","name":"MoonshotAI: Kimi K2","context_length":131072,"pricing":{"prompt":"0.0000006","completion":"0.0000025","input_cache_read":"0.00000015"},"supported_parameters":["tools"]},
 {"id":"openai/gpt-5-nano","name":"OpenAI: GPT-5 Nano","context_length":400000,"pricing":{"prompt":"0.00000005","completion":"0.0000004"},"supported_parameters":["max_tokens","tools"]},
 {"id":"openai/gpt-image","name":"OpenAI: GPT Image","context_length":400000,"pricing":{"prompt":"0.00000005","completion":"0.0000004"},"supported_parameters":["max_tokens"]},
 {"id":"openrouter/auto","name":"Auto Router","context_length":2000000,"pricing":{"prompt":"-1","completion":"-1"},"supported_parameters":["tools"]}
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

// The account's own listing is read, which leaves out what its privacy settings exclude. A model
// that takes no tools, or whose price OpenRouter states as -1, known only once a call is made, is
// not listed.
func TestTheModelsOfTheAccountThatTakeToolsAreListedWithTheirPricePerMillionTokens(t *testing.T) {
	server, got := fake(t, http.StatusOK, listing)
	models, err := New(server.URL, "or-key", zap.NewNop()).Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ServedModel{
		{ID: "moonshotai/kimi-k2", Name: "MoonshotAI: Kimi K2", ContextLength: 131072, InputUSDPerMTok: 0.6, OutputUSDPerMTok: 2.5},
		{ID: "openai/gpt-5-nano", Name: "OpenAI: GPT-5 Nano", ContextLength: 400000, InputUSDPerMTok: 0.05, OutputUSDPerMTok: 0.4},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
	if r := (*got)[0]; r.URL.Path != "/v1/models/user" || r.Header.Get("Authorization") != "Bearer or-key" {
		t.Fatalf("asked %s with Authorization %q, want the account's own listing", r.URL.Path, r.Header.Get("Authorization"))
	}
}

// The listing is read again once it is older than cacheFor, and not before.
func TestAListingIsAnsweredAgainForAFewMinutesOnly(t *testing.T) {
	server, got := fake(t, http.StatusOK, listing)
	c := New(server.URL, "or-key", zap.NewNop())
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
		"a failure": "",
		"no JSON":   "not json",
	} {
		status := http.StatusOK
		if answer == "" {
			status = http.StatusBadGateway
		}
		api, _ := fake(t, status, answer)
		if models, err := New(api.URL, "or-key", zap.NewNop()).Models(context.Background()); err == nil {
			t.Errorf("%s: models = %+v, want an error", name, models)
		}
	}
}

// A model listed at a price that is no number is left out and logged, and the rest are listed.
func TestAModelListedAtAPriceOfNoNumberIsLeftOutAndTheRestListed(t *testing.T) {
	api, _ := fake(t, http.StatusOK, `{"data":[
 {"id":"a/b","pricing":{"prompt":"free","completion":"0"},"supported_parameters":["tools"]},
 {"id":"c/d","pricing":{"prompt":"0.000001","completion":"0.000002"},"supported_parameters":["tools"]}
]}`)
	core, logs := observer.New(zap.ErrorLevel)
	models, err := New(api.URL, "or-key", zap.New(core)).Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "c/d" {
		t.Fatalf("models = %+v, %v, want c/d alone", models, err)
	}
	if entries := logs.All(); len(entries) != 1 || entries[0].ContextMap()["model"] != "a/b" {
		t.Fatalf("logged %+v, want one error naming a/b", entries)
	}
}

// A call costs what OpenRouter's answer states, and an answer that states none cannot be priced.
func TestACallCostsWhatTheAnswerStates(t *testing.T) {
	c := New("http://unused.invalid", "or-key", zap.NewNop())
	stated := 0.0123
	if cost, err := c.Cost("a/b", domain.ModelTokens{Input: 10}, &stated); err != nil || cost != stated {
		t.Errorf("cost = %v, %v, want %v", cost, err, stated)
	}
	if cost, err := c.Cost("a/b", domain.ModelTokens{Input: 10}, nil); err == nil {
		t.Errorf("no cost stated: cost = %v, want an error", cost)
	}
}

// Callers that ask while the listing is read wait for that one read, and one that gives up does
// not cut it short for the others.
func TestCallersAskingAtOnceShareOneRead(t *testing.T) {
	release := make(chan struct{})
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		<-release
		_, _ = io.WriteString(w, listing)
	}))
	t.Cleanup(server.Close)
	c := New(server.URL, "or-key", zap.NewNop())
	impatient, giveUp := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, err := c.Models(impatient)
		gaveUp <- err
	}()
	answers := make(chan error, 3)
	for range 3 {
		go func() {
			models, err := c.Models(context.Background())
			if err == nil && len(models) != 2 {
				err = errors.New("not the listing")
			}
			answers <- err
		}()
	}
	for reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	giveUp()
	if err := <-gaveUp; err == nil {
		t.Error("a caller that gave up was answered")
	}
	close(release)
	for range 3 {
		if err := <-answers; err != nil {
			t.Fatal(err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("read %d times, want once", n)
	}
}
