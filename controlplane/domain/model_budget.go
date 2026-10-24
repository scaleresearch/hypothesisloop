package domain

import (
	"errors"
	"fmt"
	"sort"
)

// ModelPrice is what a provider charges for one model, in US dollars per million tokens of each
// kind it counts. Writing to the cache is priced at the one-hour cache's rate, the one
// the platform's agents write at.
type ModelPrice struct {
	InputUSDPerMTok      float64 `yaml:"input_usd_per_mtok" json:"input_usd_per_mtok"`
	OutputUSDPerMTok     float64 `yaml:"output_usd_per_mtok" json:"output_usd_per_mtok"`
	CacheWriteUSDPerMTok float64 `yaml:"cache_write_usd_per_mtok" json:"cache_write_usd_per_mtok"`
	CacheReadUSDPerMTok  float64 `yaml:"cache_read_usd_per_mtok" json:"cache_read_usd_per_mtok"`
}

// ModelCatalog is every model of one provider a sandboxed agent or coordinator may call through
// the platform, by the model id a call states, with its price.
type ModelCatalog map[string]ModelPrice

// Validate refuses a catalog with no model, an id no model reference of provider holds, or a price
// that is not positive. The cache write price is required of a provider whose calls write to the
// cache, cacheWrites, and refused of any other, which never counts one.
func (c ModelCatalog) Validate(provider string, cacheWrites bool) error {
	if len(c) == 0 {
		return errors.New("the model catalog names no model")
	}
	for name, p := range c {
		if _, err := NewModelRef(provider, name); err != nil {
			return fmt.Errorf("model %q: %w", name, err)
		}
		if p.InputUSDPerMTok <= 0 || p.OutputUSDPerMTok <= 0 || p.CacheReadUSDPerMTok <= 0 {
			return fmt.Errorf("model %q: the input, output and cache read prices must be positive", name)
		}
		if cacheWrites && p.CacheWriteUSDPerMTok <= 0 {
			return fmt.Errorf("model %q: the cache write price must be positive", name)
		}
		if !cacheWrites && p.CacheWriteUSDPerMTok != 0 {
			return fmt.Errorf("model %q: %s counts no cache write, so the model states no cache_write_usd_per_mtok", name, provider)
		}
	}
	return nil
}

// Names are the catalog's models, sorted.
func (c ModelCatalog) Names() []string {
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ServedModel is one model a provider serves, by the model id a call states, with what the provider
// charges for its input and output in US dollars per million tokens.
type ServedModel struct {
	ID               string
	Name             string
	ContextLength    int
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// ModelTokens is what the provider counted of one model's calls.
type ModelTokens struct {
	Model                                string
	Input, Output, CacheWrite, CacheRead int64
}

// CostUSD is what t cost at p.
func (p ModelPrice) CostUSD(t ModelTokens) float64 {
	return (float64(t.Input)*p.InputUSDPerMTok + float64(t.Output)*p.OutputUSDPerMTok +
		float64(t.CacheWrite)*p.CacheWriteUSDPerMTok + float64(t.CacheRead)*p.CacheReadUSDPerMTok) / 1e6
}

// ModelBudget is what a platform experiment may spend on model calls and what it spent.
type ModelBudget struct {
	BudgetUSD float64 `json:"model_budget_usd" doc:"What its agents and coordinator may spend on model calls the platform makes for them, in US dollars: its model_budget_usd, or the platform's default when it states none."`
	UsedUSD   float64 `json:"model_used_usd" doc:"What their model calls cost so far, the sum of the cost recorded with each call. Once it reaches model_budget_usd every further model call is refused with 429 model_budget_spent until the budget is raised."`
}

// Spent reports whether no further model call may be made.
func (b ModelBudget) Spent() bool { return b.UsedUSD >= b.BudgetUSD }

// ModelBudgetUSD is the model budget of a platform experiment that stated stated: its own, or
// platformDefault for none.
func ModelBudgetUSD(stated *float64, platformDefault float64) float64 {
	if stated != nil {
		return *stated
	}
	return platformDefault
}

// The reasons the model proxy refuses a call with.
const (
	// ReasonModelNotServed: the call or the choice names a model the platform does not serve.
	ReasonModelNotServed = "model_not_served"
	// ReasonModelNotTheCallers: the call goes to another provider than its caller's model's, or
	// names another model where the provider serves the caller's alone.
	ReasonModelNotTheCallers = "model_not_the_callers"
	// ReasonModelBudgetSpent: the platform experiment's model calls cost its model budget.
	ReasonModelBudgetSpent = "model_budget_spent"
	// ReasonModelCallsInFlight: the org has as many model calls in flight as the platform
	// makes for one org at once.
	ReasonModelCallsInFlight = "model_calls_in_flight"
	// ReasonModelBudgetTooHigh: a platform experiment states a model budget above the most the
	// platform allows one.
	ReasonModelBudgetTooHigh = "model_budget_too_high"
)
