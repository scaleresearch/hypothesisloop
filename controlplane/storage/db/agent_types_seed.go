package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Seeded is what a start's seeding wrote: each built-in type given a new version, each it archived
// for the release no longer having it, and the version of the platform's rules it made current, 0
// for none. Skipped is what an older release than the one that wrote the current version had, or
// lacked, and left alone.
type Seeded struct {
	Types        []SeededAgentType
	Archived     []string
	RulesVersion int
	RulesTold    int
	Skipped      []string
}

// SeededAgentType is a built-in type the seeding gave a new version, and whom that told.
type SeededAgentType struct {
	ID          string
	Propagation domain.Propagation
}

// SeedBuiltins writes the built-in agent types and the platform's rules as release has them,
// under the lock EnsureSchema takes, in one transaction. A type that does not exist yet is created at
// version 1. Otherwise, while release is at least that of the current version (NULL counts as
// 0), its name and description are written, an archived one is brought back, and a prompt and
// settings, or rules, that differ from the current version's become the next version, which every
// live agent is told of. A built-in type release no longer has is archived under the same rule.
// A release older than the current version's writes nothing, so an older replica of a rolling
// deploy never puts its own back; a revert ships under a newer release and is written like any
// change (agenttypes.Release).
func (s *Store) SeedBuiltins(ctx context.Context, release int, builtins []domain.BuiltinAgentType, rules string) (Seeded, error) {
	var seeded Seeded
	if err := s.write(ctx, func(tx *Write) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", schemaLockKey); err != nil {
			return fmt.Errorf("db.SeedBuiltins: lock: %w", err)
		}
		ids := make([]string, 0, len(builtins))
		for _, b := range builtins {
			ids = append(ids, b.ID)
			propagation, skipped, err := seedBuiltinTx(ctx, tx, release, b)
			if err != nil {
				return fmt.Errorf("db.SeedBuiltins: %s: %w", b.ID, err)
			}
			if skipped {
				seeded.Skipped = append(seeded.Skipped, b.ID)
			}
			if propagation != nil {
				seeded.Types = append(seeded.Types, SeededAgentType{ID: b.ID, Propagation: *propagation})
			}
		}
		if err := retireBuiltinsTx(ctx, tx, release, ids, &seeded); err != nil {
			return fmt.Errorf("db.SeedBuiltins: retire: %w", err)
		}
		skipped, err := seedRulesTx(ctx, tx, release, rules, &seeded)
		if err != nil {
			return fmt.Errorf("db.SeedBuiltins: rules: %w", err)
		}
		if skipped {
			seeded.Skipped = append(seeded.Skipped, "rules")
		}
		return nil
	}); err != nil {
		return Seeded{}, err
	}
	return seeded, nil
}

// seedBuiltinTx writes one built-in type: the propagation of the version it inserted after the
// first, nil when it inserted none, and whether release is older than the current version's.
func seedBuiltinTx(ctx context.Context, tx *Write, release int, b domain.BuiltinAgentType) (*domain.Propagation, bool, error) {
	platform := domain.Actor{Kind: domain.ActorStaff}
	tag, err := tx.Exec(ctx, `
INSERT INTO agent_types (id, org_id, name, description, role, current_version, created_by_kind) VALUES ($1, NULL, $2, $3, $4, 1, $5)
ON CONFLICT (id) DO NOTHING`, b.ID, b.Name, b.Description, string(b.Role), string(platform.Kind))
	if err != nil {
		return nil, false, err
	}
	if tag.RowsAffected() == 1 {
		if err := insertAgentTypeVersion(ctx, tx, b.ID, 1, b.Prompt, b.Settings, &release, platform); err != nil {
			return nil, false, err
		}
		return nil, false, recordEventTx(ctx, tx, b.ID, domain.AgentTypeCreated, 1, "", platform)
	}
	current, err := getAgentType(ctx, tx, "", b.ID, true)
	if errors.Is(err, ErrAgentTypeNotFound) {
		return nil, false, fmt.Errorf("an org's agent type holds the built-in id")
	}
	if err != nil {
		return nil, false, err
	}
	if current.Role != b.Role {
		return nil, false, fmt.Errorf("the built-in is of role %s and the stored one of %s: a type's role is fixed", b.Role, current.Role)
	}
	if current.Current.Release != nil && release < *current.Current.Release {
		return nil, true, nil
	}
	if current.ArchivedAt != nil {
		if _, err := tx.Exec(ctx, `UPDATE agent_types SET archived_at = NULL WHERE id = $1`, b.ID); err != nil {
			return nil, false, err
		}
		if err := recordEventTx(ctx, tx, b.ID, domain.AgentTypeCreated, current.CurrentVersion, fmt.Sprintf("release %d", release), platform); err != nil {
			return nil, false, err
		}
	}
	if err := describeTx(ctx, tx, current, b.Name, b.Description, platform); err != nil {
		return nil, false, err
	}
	digest, err := domain.AgentTypeDigest(b.Prompt, b.Settings)
	if err != nil {
		return nil, false, err
	}
	if digest == current.Current.Digest {
		return nil, false, nil
	}
	propagation, err := newVersionTx(ctx, tx, current, b.Prompt, b.Settings, &release, domain.AgentTypeEdited, fmt.Sprintf("release %d", release), platform)
	return propagation, false, err
}

// retireBuiltinsTx archives each built-in type that is not archived and whose id is none of ids,
// while release is at least that of its current version, as an org's archive does and with no
// check of who runs it: the agents that do keep running.
func retireBuiltinsTx(ctx context.Context, tx *Write, release int, ids []string, seeded *Seeded) error {
	rows, err := tx.Query(ctx, `SELECT id FROM agent_types WHERE org_id IS NULL AND archived_at IS NULL AND id <> ALL($1) ORDER BY id`, ids)
	if err != nil {
		return err
	}
	retired, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range retired {
		current, err := getAgentType(ctx, tx, "", id, true)
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if current.Current.Release != nil && release < *current.Current.Release {
			seeded.Skipped = append(seeded.Skipped, id)
			continue
		}
		if err := archiveTx(ctx, tx, current, fmt.Sprintf("release %d", release), domain.Actor{Kind: domain.ActorStaff}); err != nil {
			return err
		}
		seeded.Archived = append(seeded.Archived, id)
	}
	return nil
}

// seedRulesTx makes rules the next version of the platform's rules when they differ from the
// current one and release is at least its, and tells every live agent and coordinator. It answers
// whether release is older than the current version's.
func seedRulesTx(ctx context.Context, tx *Write, release int, rules string, seeded *Seeded) (bool, error) {
	digest, err := domain.AgentTypeDigest(rules, domain.AgentTypeSettings{})
	if err != nil {
		return false, err
	}
	var version, currentRelease int
	var currentDigest string
	err = tx.QueryRow(ctx, `SELECT version, digest, release FROM platform_rules ORDER BY version DESC LIMIT 1`).Scan(&version, &currentDigest, &currentRelease)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil && release < currentRelease {
		return true, nil
	}
	if err == nil && digest == currentDigest {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_rules (version, rules, digest, release) VALUES ($1, $2, $3, $4)`, version+1, rules, digest, release); err != nil {
		return false, err
	}
	told, err := tellTx(ctx, tx, `true`, `true`)
	if err != nil {
		return false, err
	}
	seeded.RulesVersion, seeded.RulesTold = version+1, told
	return false, nil
}
