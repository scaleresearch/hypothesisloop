package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// URL is where OpenRouter's API is reached.
const URL = "https://openrouter.ai/api"

// cacheFor is how long a listing read from OpenRouter is answered again before it is read anew.
// OpenRouter is a third party, and every choice of a model and every GET /models reads it.
const cacheFor = 2 * time.Minute

// maxListingBytes bounds the listing read, which is about a megabyte.
const maxListingBytes = 32 << 20

// Catalog reads the models OpenRouter serves the account that take tools, as Claude Code calls them.
type Catalog struct {
	url    string
	apiKey string
	client *http.Client
	logger *zap.Logger
	now    func() time.Time

	mu      sync.Mutex
	read    time.Time
	listing []domain.ServedModel
	// reading is the read of the listing in flight, nil for none: every caller meanwhile waits
	// for it.
	reading *reading
}

type reading struct {
	done    chan struct{}
	listing []domain.ServedModel
	err     error
}

// New is the catalog of OpenRouter's API at url as the account of apiKey reads it.
func New(url, apiKey string, logger *zap.Logger) *Catalog {
	return &Catalog{url: url, apiKey: apiKey, client: &http.Client{Timeout: 20 * time.Second}, logger: logger, now: time.Now}
}

// Models is every model OpenRouter serves the account that takes tools at a price it states, read
// at most cacheFor ago. The account's own listing leaves out the models its privacy and provider
// settings exclude, whose every call OpenRouter refuses with 400.
// One read is in flight at a time, made with no caller's cancellation, and the lock is not held
// across it.
func (c *Catalog) Models(ctx context.Context) ([]domain.ServedModel, error) {
	c.mu.Lock()
	if c.listing != nil && c.now().Sub(c.read) < cacheFor {
		defer c.mu.Unlock()
		return c.listing, nil
	}
	r := c.reading
	if r == nil {
		r = &reading{done: make(chan struct{})}
		c.reading = r
		go c.readListing(context.WithoutCancel(ctx), r)
	}
	c.mu.Unlock()
	select {
	case <-r.done:
		return r.listing, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("openrouter: list models: %w", ctx.Err())
	}
}

func (c *Catalog) readListing(ctx context.Context, r *reading) {
	r.listing, r.err = c.fetch(ctx)
	c.mu.Lock()
	if r.err == nil {
		c.listing, c.read = r.listing, c.now()
	}
	c.reading = nil
	c.mu.Unlock()
	close(r.done)
}

func (c *Catalog) fetch(ctx context.Context) ([]domain.ServedModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/v1/models/user", nil)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxListingBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("openrouter: read the models listed: %w", err)
	}
	models := make([]domain.ServedModel, 0, len(body.Data))
	for _, m := range body.Data {
		// The account's listing takes no supported_parameters filter.
		if !slices.Contains(m.SupportedParameters, "tools") {
			continue
		}
		input, inputErr := perMTok(m.Pricing.Prompt)
		output, outputErr := perMTok(m.Pricing.Completion)
		if err := errors.Join(inputErr, outputErr); err != nil {
			c.logger.Error("openrouter: a model listed at a price that is no number is not served", zap.String("model", m.ID), zap.Error(err))
			continue
		}
		// A negative price is one OpenRouter learns only from the call, such as its router's: no
		// budget holds a call of such a model in advance, so no agent runs on one.
		if input < 0 || output < 0 {
			continue
		}
		models = append(models, domain.ServedModel{ID: m.ID, Name: m.Name, ContextLength: m.ContextLength, InputUSDPerMTok: input, OutputUSDPerMTok: output})
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

// Cost is what OpenRouter's answer to a call states the call cost, which no listed price tells: it
// varies with the provider OpenRouter routes the call to and the length of its prompt.
func (*Catalog) Cost(_ string, _ domain.ModelTokens, stated *float64) (float64, error) {
	if stated == nil {
		return 0, errors.New("openrouter's answer states no cost")
	}
	return *stated, nil
}
