package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// URL is where OpenRouter's API is reached.
const URL = "https://openrouter.ai/api"

// cacheFor is how long a listing read from OpenRouter is answered again before it is read anew.
// OpenRouter is a third party, and every choice of a model and every GET /models reads it.
const cacheFor = 2 * time.Minute

// maxListingBytes bounds the listing read, which is about a megabyte.
const maxListingBytes = 32 << 20

// Model is one model OpenRouter serves, with what it charges for it.
type Model struct {
	ID               string
	Name             string
	ContextLength    int
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// Catalog reads the models OpenRouter serves that take tools, as Claude Code calls them.
type Catalog struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	read    time.Time
	listing []Model
}

// New is the catalog of OpenRouter's API at url.
func New(url string) *Catalog {
	return &Catalog{url: url, client: &http.Client{Timeout: 20 * time.Second}, now: time.Now}
}

// Models is every model OpenRouter serves that takes tools at a price it states, read at most
// cacheFor ago.
func (c *Catalog) Models(ctx context.Context) ([]Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listing != nil && c.now().Sub(c.read) < cacheFor {
		return c.listing, nil
	}
	listing, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	c.listing, c.read = listing, c.now()
	return listing, nil
}

func (c *Catalog) fetch(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/v1/models?supported_parameters=tools", nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter: list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter: list models: %s", resp.Status)
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListingBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("openrouter: read the models listed: %w", err)
	}
	models := make([]Model, 0, len(body.Data))
	for _, m := range body.Data {
		input, err := perMTok(m.Pricing.Prompt)
		if err != nil {
			return nil, fmt.Errorf("openrouter: model %s: prompt price: %w", m.ID, err)
		}
		output, err := perMTok(m.Pricing.Completion)
		if err != nil {
			return nil, fmt.Errorf("openrouter: model %s: completion price: %w", m.ID, err)
		}
		// A negative price is one OpenRouter learns only from the call, such as its router's: no
		// budget holds a call of such a model in advance, so no agent runs on one.
		if input < 0 || output < 0 {
			continue
		}
		models = append(models, Model{ID: m.ID, Name: m.Name, ContextLength: m.ContextLength, InputUSDPerMTok: input, OutputUSDPerMTok: output})
	}
	return models, nil
}

// perMTok is a price OpenRouter states in US dollars per token, per million tokens, rounded to
// a billionth of a dollar to drop what the float product adds.
func perMTok(perToken string) (float64, error) {
	usd, err := strconv.ParseFloat(perToken, 64)
	if err != nil {
		return 0, err
	}
	return math.Round(usd*1e15) / 1e9, nil
}
