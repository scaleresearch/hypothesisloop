package metricsdb

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// ModelCall is what the provider counted for one model call, and what it cost.
type ModelCall struct {
	OrgID                    string
	PlatformExperimentID     string
	AgentID                  string
	Provider                 string
	Model                    string
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	CostUSD                  float64
	At                       time.Time
}

// RecordModelCall stores one model call.
func (s *Store) RecordModelCall(ctx context.Context, c ModelCall) error {
	if c.OrgID == "" || c.PlatformExperimentID == "" || c.AgentID == "" || c.Provider == "" {
		return fmt.Errorf("metricsdb.RecordModelCall: org, platform experiment, agent and provider are required")
	}
	insert := fmt.Sprintf(`INSERT INTO model_calls (org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, cost_usd, ts) VALUES (%s, %s, %s, %s, %s, %d, %d, %d, %d, %s, %d)`,
		sqlQuote(c.OrgID), sqlQuote(c.PlatformExperimentID), sqlQuote(c.AgentID), sqlQuote(c.Provider), sqlQuote(c.Model),
		c.InputTokens, c.OutputTokens, c.CacheCreationInputTokens, c.CacheReadInputTokens, strconv.FormatFloat(c.CostUSD, 'g', -1, 64), c.At.UnixMilli())
	if err := s.execSQL(ctx, insert); err != nil {
		return fmt.Errorf("metricsdb.RecordModelCall: %w", err)
	}
	return nil
}

// AgentModelUsage is the sum of the model calls of one agent on one model.
type AgentModelUsage struct {
	AgentID                  string  `json:"agent_id"`
	Provider                 string  `json:"provider"`
	Model                    string  `json:"model"`
	Calls                    int64   `json:"calls"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CostUSD                  float64 `json:"cost_usd" doc:"What the calls cost, in US dollars."`
}

// ModelUsageOf sums the model calls made for one platform experiment, per agent and model, each
// at the cost recorded with it.
func (s *Store) ModelUsageOf(ctx context.Context, platformExperimentID string) ([]AgentModelUsage, error) {
	const op = "metricsdb.ModelUsageOf"
	rows, err := s.querySQLRows(ctx, `SELECT agent_id, provider, model, COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens), SUM(cost_usd)
FROM model_calls WHERE platform_experiment_id = `+sqlQuote(platformExperimentID)+` GROUP BY agent_id, provider, model ORDER BY agent_id, provider, model`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	usage := make([]AgentModelUsage, 0, len(rows))
	for _, row := range rows {
		if len(row) != 9 {
			return nil, fmt.Errorf("%s: a row of %d columns, want 9", op, len(row))
		}
		u := AgentModelUsage{}
		var ok bool
		if u.AgentID, ok = row[0].(string); !ok {
			return nil, fmt.Errorf("%s: agent_id is %T", op, row[0])
		}
		if u.Provider, ok = row[1].(string); !ok {
			return nil, fmt.Errorf("%s: provider is %T", op, row[1])
		}
		if u.Model, ok = row[2].(string); !ok {
			return nil, fmt.Errorf("%s: model is %T", op, row[2])
		}
		for i, field := range []*int64{&u.Calls, &u.InputTokens, &u.OutputTokens, &u.CacheCreationInputTokens, &u.CacheReadInputTokens} {
			n, ok := row[3+i].(float64)
			if !ok {
				return nil, fmt.Errorf("%s: column %d is %T", op, 3+i, row[3+i])
			}
			*field = int64(n)
		}
		if u.CostUSD, ok = row[8].(float64); !ok {
			return nil, fmt.Errorf("%s: cost_usd is %T", op, row[8])
		}
		usage = append(usage, u)
	}
	return usage, nil
}
