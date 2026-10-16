package metricsdb

import (
	"context"
	"fmt"
	"time"
)

// model_usage holds one row per model call a hosted agent made through the control plane's model
// proxy: what the provider counted for it, under the org, platform experiment and agent it was
// made for. append_mode keeps two calls of one agent on one model in the same millisecond as two
// rows, where a primary key and a timestamp alike would otherwise keep the last.
const ensureModelUsageTableSQL = `CREATE TABLE IF NOT EXISTS model_usage (
	org_id STRING,
	platform_experiment_id STRING,
	agent_id STRING,
	provider STRING,
	model STRING,
	input_tokens BIGINT,
	output_tokens BIGINT,
	cache_creation_input_tokens BIGINT,
	cache_read_input_tokens BIGINT,
	ts TIMESTAMP(3) TIME INDEX,
	PRIMARY KEY (org_id, platform_experiment_id, agent_id, provider, model)
) WITH ('append_mode' = 'true')`

// ModelCall is what the provider counted for one model call.
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
	At                       time.Time
}

// RecordModelCall stores one model call.
func RecordModelCall(ctx context.Context, dbURL string, c ModelCall) error {
	if c.OrgID == "" || c.PlatformExperimentID == "" || c.AgentID == "" || c.Provider == "" {
		return fmt.Errorf("metricsdb.RecordModelCall: org, platform experiment, agent and provider are required")
	}
	insert := fmt.Sprintf(`INSERT INTO model_usage (org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, ts) VALUES (%s, %s, %s, %s, %s, %d, %d, %d, %d, %d)`,
		sqlQuote(c.OrgID), sqlQuote(c.PlatformExperimentID), sqlQuote(c.AgentID), sqlQuote(c.Provider), sqlQuote(c.Model),
		c.InputTokens, c.OutputTokens, c.CacheCreationInputTokens, c.CacheReadInputTokens, c.At.UnixMilli())
	if err := execSQL(ctx, dbURL, insert); err != nil {
		return fmt.Errorf("metricsdb.RecordModelCall: %w", err)
	}
	return nil
}

// AgentModelUsage is the sum of the model calls of one agent on one model.
type AgentModelUsage struct {
	AgentID                  string `json:"agent_id"`
	Provider                 string `json:"provider"`
	Model                    string `json:"model"`
	Calls                    int64  `json:"calls"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
}

// ModelUsageOf sums the model calls made for one platform experiment, per agent and model.
func ModelUsageOf(ctx context.Context, dbURL, platformExperimentID string) ([]AgentModelUsage, error) {
	rows, err := querySQLRows(ctx, dbURL, fmt.Sprintf(`SELECT agent_id, provider, model, COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_input_tokens), SUM(cache_read_input_tokens)
FROM model_usage WHERE platform_experiment_id = %s GROUP BY agent_id, provider, model ORDER BY agent_id, provider, model`, sqlQuote(platformExperimentID)))
	if err != nil {
		return nil, fmt.Errorf("metricsdb.ModelUsageOf: %w", err)
	}
	usage := make([]AgentModelUsage, 0, len(rows))
	for _, row := range rows {
		if len(row) != 8 {
			return nil, fmt.Errorf("metricsdb.ModelUsageOf: a row of %d columns, want 8", len(row))
		}
		u := AgentModelUsage{}
		var ok bool
		if u.AgentID, ok = row[0].(string); !ok {
			return nil, fmt.Errorf("metricsdb.ModelUsageOf: agent_id is %T", row[0])
		}
		if u.Provider, ok = row[1].(string); !ok {
			return nil, fmt.Errorf("metricsdb.ModelUsageOf: provider is %T", row[1])
		}
		if u.Model, ok = row[2].(string); !ok {
			return nil, fmt.Errorf("metricsdb.ModelUsageOf: model is %T", row[2])
		}
		for i, field := range []*int64{&u.Calls, &u.InputTokens, &u.OutputTokens, &u.CacheCreationInputTokens, &u.CacheReadInputTokens} {
			n, ok := row[3+i].(float64)
			if !ok {
				return nil, fmt.Errorf("metricsdb.ModelUsageOf: column %d is %T", 3+i, row[3+i])
			}
			*field = int64(n)
		}
		usage = append(usage, u)
	}
	return usage, nil
}
