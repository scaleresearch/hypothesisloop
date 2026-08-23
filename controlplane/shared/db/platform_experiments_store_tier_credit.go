package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// participantTiers resolves each named agent's quota tier from its kind and its signup-time
// override, on the transaction's own connection. Reading it here rather than accepting it from
// the caller is what keeps the tier a property of the signup: a caller passing its own idea of
// who is burst-only would eventually pass a stale one.
func participantTiers(ctx context.Context, tx pgx.Tx, platformExpID string, agentIDs []string) (map[string]domain.QuotaTier, error) {
	rows, err := tx.Query(ctx, `
SELECT s.agent_id, COALESCE(a.kind, ''), s.quota_tier
FROM experiment_signups s
LEFT JOIN agents a ON a.id = s.agent_id
WHERE s.platform_experiment_id = $1 AND s.agent_id = ANY($2)`, platformExpID, agentIDs)
	if err != nil {
		return nil, fmt.Errorf("participantTiers: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.QuotaTier, len(agentIDs))
	for rows.Next() {
		var agentID string
		var kind domain.AgentKind
		var override domain.QuotaTier
		if err := rows.Scan(&agentID, &kind, &override); err != nil {
			return nil, fmt.Errorf("participantTiers: scan: %w", err)
		}
		out[agentID] = domain.ResolveQuotaTier(kind, override)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("participantTiers: %w", err)
	}
	return out, nil
}

// creditIntoTier adds guaranteedAmount/burstAmount to every named agent, routed through the tier
// that agent's signup entitles it to. Every path that grows an allocation after the initial one
// goes through here.
//
// The hard-learned part: crediting the guaranteed column directly, which both the stage-boundary
// release and donation fulfilment used to do, hands guaranteed quota to a burst-only participant.
// Start is careful to give an agent none, and then every boundary it survived gave some back — so
// the tier held for exactly as long as nothing happened.
//
// An agent named here with no signup row is an error, not a skip: it would otherwise be credited
// under whatever tier the zero value resolves to, which is the one thing this exists to prevent.
func creditIntoTier(ctx context.Context, tx pgx.Tx, platformExpID string, agentIDs []string,
	guaranteedCol, burstCol string, guaranteedAmount, burstAmount float64) error {
	if len(agentIDs) == 0 || (guaranteedAmount == 0 && burstAmount == 0) {
		return nil
	}
	tiers, err := participantTiers(ctx, tx, platformExpID, agentIDs)
	if err != nil {
		return err
	}
	// Only two outcomes are possible, so the credit is at most two statements regardless of how
	// many agents a boundary releases into.
	byTier := map[domain.QuotaTier][]string{}
	for _, agentID := range agentIDs {
		tier, ok := tiers[agentID]
		if !ok {
			return fmt.Errorf("creditIntoTier: agent %s has no signup in %s", agentID, platformExpID)
		}
		byTier[tier] = append(byTier[tier], agentID)
	}
	q := fmt.Sprintf(
		`UPDATE agent_quotas SET %[1]s = %[1]s + $3, %[2]s = %[2]s + $4 WHERE platform_experiment_id=$1 AND agent_id = ANY($2)`,
		guaranteedCol, burstCol)
	for tier, ids := range byTier {
		g, b := domain.ApplyQuotaTier(tier, guaranteedAmount, burstAmount)
		if _, err := tx.Exec(ctx, q, platformExpID, ids, g, b); err != nil {
			return fmt.Errorf("creditIntoTier: credit %s tier: %w", tier, err)
		}
	}
	return nil
}
