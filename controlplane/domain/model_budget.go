package domain

import (
	"errors"
	"fmt"
	"sort"
)

// ModelPrice is what the model provider charges for one model, in US dollars per million tokens
// of each kind it counts. Writing to the cache is priced at the one-hour cache's rate, the one
// the platform's agents write at.
type ModelPrice struct {
	InputUSDPerMTok      float64 `yaml:"input_usd_per_mtok" json:"input_usd_per_mtok"`
	OutputUSDPerMTok     float64 `yaml:"output_usd_per_mtok" json:"output_usd_per_mtok"`
	CacheWriteUSDPerMTok float64 `yaml:"cache_write_usd_per_mtok" json:"cache_write_usd_per_mtok"`
	CacheReadUSDPerMTok  float64 `yaml:"cache_read_usd_per_mtok" json:"cache_read_usd_per_mtok"`
}

// ModelCatalog is every model a sandboxed agent or coordinator may call through the platform,
// by the name a call states, with its price.
type ModelCatalog map[string]ModelPrice

// Validate refuses a catalog with no model, a name no agent can be started on, or a price that
// is not positive.
func (c ModelCatalog) Validate() error {
	if len(c) == 0 {
		return errors.New("the model catalog names no model")
	}
	for name, p := range c {
		if err := ValidateModel(name); err != nil {
			return fmt.Errorf("model %q: %w", name, err)
		}
		if p.InputUSDPerMTok <= 0 || p.OutputUSDPerMTok <= 0 || p.CacheWriteUSDPerMTok <= 0 || p.CacheReadUSDPerMTok <= 0 {
			return fmt.Errorf("model %q: every price must be positive", name)
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

// ModelTokens is what the provider counted of one model's calls.
type ModelTokens struct {
	Model                                string
	Input, Output, CacheWrite, CacheRead int64
}

// SpendUSD is what used cost at the catalog's prices. A model the catalog does not price is an
// error: no call to it was served, so its usage was recorded under a catalog that priced it.
func (c ModelCatalog) SpendUSD(used []ModelTokens) (float64, error) {
	var usd float64
	for _, u := range used {
		p, ok := c[u.Model]
		if !ok {
			return 0, fmt.Errorf("model %q has usage and no price in the model catalog", u.Model)
		}
		usd += (float64(u.Input)*p.InputUSDPerMTok + float64(u.Output)*p.OutputUSDPerMTok +
			float64(u.CacheWrite)*p.CacheWriteUSDPerMTok + float64(u.CacheRead)*p.CacheReadUSDPerMTok) / 1e6
	}
	return usd, nil
}

// ModelBudget is what a platform experiment may spend on model calls and what it spent.
type ModelBudget struct {
	BudgetUSD float64 `json:"model_budget_usd" doc:"What its agents and coordinator may spend on model calls the platform makes for them, in US dollars: its model_budget_usd, or the platform's default when it states none."`
	UsedUSD   float64 `json:"model_used_usd" doc:"What their model calls cost so far, at the platform's prices. Once it reaches model_budget_usd every further model call is refused with 429 model_budget_spent until the budget is raised."`
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
	// ReasonModelNotServed: the call names a model outside the platform's model catalog.
	ReasonModelNotServed = "model_not_served"
	// ReasonModelBudgetSpent: the platform experiment's model calls cost its model budget.
	ReasonModelBudgetSpent = "model_budget_spent"
	// ReasonModelCallsInFlight: the org has as many model calls in flight as the platform
	// makes for one org at once.
	ReasonModelCallsInFlight = "model_calls_in_flight"
	// ReasonModelBudgetTooHigh: a platform experiment states a model budget above the most the
	// platform allows one.
	ReasonModelBudgetTooHigh = "model_budget_too_high"
)
