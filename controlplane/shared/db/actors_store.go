package db

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// NameActors sets on each actor the name it carries now: a person's display name, an API key's
// name, an agent's name. One that has none, or is no longer there, is named by its id. A
// coordinator and a staff token are named by what they are, and an unverified actor keeps the
// name its record stated. It is one read however many actors are given.
func (s *AgentsStore) NameActors(ctx context.Context, actors []*domain.Actor) error {
	ids := map[domain.ActorKind][]string{}
	for _, a := range actors {
		switch a.Kind {
		case domain.ActorUser, domain.ActorKey, domain.ActorAgent:
			ids[a.Kind] = append(ids[a.Kind], a.ID)
			a.Name = a.ID
		case domain.ActorCoordinator, domain.ActorStaff:
			a.Name = string(a.Kind)
		case domain.ActorUnverified:
		default:
			return fmt.Errorf("agents_store.NameActors: actor of unknown kind %q", a.Kind)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.pool.pool.Query(ctx, `
SELECT 'user', id, name FROM users WHERE id = ANY($1) AND name <> ''
UNION ALL
SELECT 'key', id, name FROM api_keys WHERE id = ANY($2) AND name <> ''
UNION ALL
SELECT 'agent', id, name FROM agents WHERE id = ANY($3) AND name <> ''`,
		ids[domain.ActorUser], ids[domain.ActorKey], ids[domain.ActorAgent])
	if err != nil {
		return fmt.Errorf("agents_store.NameActors: %w", err)
	}
	defer rows.Close()
	type ref struct {
		kind domain.ActorKind
		id   string
	}
	names := map[ref]string{}
	for rows.Next() {
		var r ref
		var name string
		if err := rows.Scan(&r.kind, &r.id, &name); err != nil {
			return fmt.Errorf("agents_store.NameActors: scan: %w", err)
		}
		names[r] = name
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agents_store.NameActors: %w", err)
	}
	for _, a := range actors {
		if name, ok := names[ref{a.Kind, a.ID}]; ok {
			a.Name = name
		}
	}
	return nil
}
