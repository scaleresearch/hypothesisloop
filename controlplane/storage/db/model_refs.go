package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// bare is true of a model stored before models were references (domain.ModelRef): a name with no
// provider, which no reference is, and every one of them an Anthropic model.
func bare(model string) string {
	return fmt.Sprintf(`(%[1]s <> '' AND strpos(%[1]s, '/') = 0)`, model)
}

// prefixed is the JSONB array entries with each entry's bare model made anthropic/<model>.
func prefixed(entries string) string {
	model := "COALESCE(e->>'model', '')"
	return `(SELECT jsonb_agg(CASE WHEN ` + bare(model) + ` THEN jsonb_set(e, '{model}', to_jsonb('anthropic/' || ` + model + `)) ELSE e END ORDER BY i)
FROM jsonb_array_elements(` + entries + `) WITH ORDINALITY AS entries(e, i))`
}

// holdsBare is true of JSONB array entries of which one names a bare model.
func holdsBare(entries string) string {
	return `EXISTS (SELECT FROM jsonb_array_elements(` + entries + `) e WHERE ` + bare("COALESCE(e->>'model', '')") + `)`
}

// prefixBareModels makes every model the database stores a reference: anthropic/ before each bare
// one, in signups, coordinators, agent pools, proposals and agent type versions, whose digest
// follows their settings. It changes nothing on a database that holds none.
func prefixBareModels(ctx context.Context, tx pgx.Tx) error {
	for _, update := range []string{
		`UPDATE experiment_signups SET model = 'anthropic/' || model WHERE ` + bare("model"),
		`UPDATE platform_experiments SET coordinator_model = 'anthropic/' || coordinator_model WHERE ` + bare("coordinator_model"),
		`UPDATE platform_experiments SET agents = ` + prefixed("agents") + ` WHERE ` + holdsBare("agents"),
		`UPDATE platform_experiments SET proposal = jsonb_set(proposal, '{agents}', ` + prefixed("proposal->'agents'") + `)
WHERE jsonb_typeof(proposal->'agents') = 'array' AND ` + holdsBare("proposal->'agents'"),
	} {
		if _, err := tx.Exec(ctx, update); err != nil {
			return fmt.Errorf("prefix bare models: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT type_id, version, prompt, settings FROM agent_type_versions WHERE `+bare("COALESCE(settings->>'model', '')")+` FOR UPDATE`)
	if err != nil {
		return fmt.Errorf("prefix bare models: read agent type versions: %w", err)
	}
	type version struct {
		typeID   string
		number   int
		prompt   string
		settings domain.AgentTypeSettings
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (version, error) {
		var v version
		var settings []byte
		if err := row.Scan(&v.typeID, &v.number, &v.prompt, &settings); err != nil {
			return v, err
		}
		return v, json.Unmarshal(settings, &v.settings)
	})
	if err != nil {
		return fmt.Errorf("prefix bare models: read agent type versions: %w", err)
	}
	for _, v := range versions {
		v.settings.Model = domain.ModelProviderAnthropic + "/" + v.settings.Model
		digest, err := domain.AgentTypeDigest(v.prompt, v.settings)
		if err != nil {
			return fmt.Errorf("prefix bare models: %s version %d: %w", v.typeID, v.number, err)
		}
		encoded, err := json.Marshal(v.settings)
		if err != nil {
			return fmt.Errorf("prefix bare models: %s version %d: %w", v.typeID, v.number, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_type_versions SET settings = $3, digest = $4 WHERE type_id = $1 AND version = $2`, v.typeID, v.number, encoded, digest); err != nil {
			return fmt.Errorf("prefix bare models: %s version %d: %w", v.typeID, v.number, err)
		}
	}
	return nil
}
