package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	// ErrAgentEnded is a message to an agent that finished or was cut: nothing would read it.
	ErrAgentEnded = errors.New("the agent's run has ended")
	// ErrNoResearcherAtWork is a message to every researcher when none is at work.
	ErrNoResearcherAtWork = errors.New("no researcher is at work")
)

// MessagesRateLimited refuses a message to an agent already sent the most an hour allows.
type MessagesRateLimited struct {
	AgentID string
	// Next is when the oldest message of the hour leaves it, and the next may be sent.
	Next time.Time
}

func (e *MessagesRateLimited) Error() string {
	return fmt.Sprintf("agent %s was sent %d messages in the last hour", e.AgentID, domain.MaxAgentMessagesPerHour)
}

// SendAgentMessages stores text as a message from sender to each agent of to, or with to empty to
// every researcher at work, and returns them, or stores none: ErrPlatformExperimentClosed once the
// platform experiment closed, ErrNotSignedUp for an agent not in it, ErrAgentEnded for one that
// finished or was cut, ErrNoResearcherAtWork, and *MessagesRateLimited for an agent sent
// domain.MaxAgentMessagesPerHour in the last hour. Each is told as agent.message.
func (s *PlatformExperimentsStore) SendAgentMessages(ctx context.Context, platformExpID string, to []string, text string, sender domain.Actor) ([]domain.AgentMessage, error) {
	tx, err := s.pool.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.SendAgentMessages: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	err = tx.QueryRow(ctx, `SELECT status::text FROM platform_experiments WHERE id = $1 FOR SHARE`, platformExpID).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNotSignedUp
	case err != nil:
		return nil, fmt.Errorf("platform_experiments_store.SendAgentMessages: %w", err)
	case status == string(domain.PlatformExpClosed):
		return nil, ErrPlatformExperimentClosed
	}

	if to == nil {
		to = []string{}
	}
	// The signups are locked so that two sends to one agent count each other.
	rows, err := tx.Query(ctx, `
SELECT s.agent_id, s.finished_at IS NOT NULL OR EXISTS (
           SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id),
       (SELECT array_agg(m.created_at ORDER BY m.created_at) FROM agent_messages m
        WHERE m.platform_experiment_id = s.platform_experiment_id AND m.agent_id = s.agent_id AND m.created_at > now() - interval '1 hour')
FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.role <> $2 AND (cardinality($3::text[]) = 0 AND s.role = $4 OR s.agent_id = ANY($3))
ORDER BY s.signed_up_at, s.agent_id
FOR UPDATE OF s`, platformExpID, string(domain.SignupRolePlanner), to, string(domain.SignupRoleCompetitor))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.SendAgentMessages: %w", err)
	}
	type recipient struct {
		agentID string
		ended   bool
		recent  []time.Time
	}
	recipients, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (recipient, error) {
		var r recipient
		err := row.Scan(&r.agentID, &r.ended, &r.recent)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.SendAgentMessages: %w", err)
	}
	found := map[string]bool{}
	var kept []recipient
	for _, r := range recipients {
		found[r.agentID] = true
		switch {
		case r.ended && len(to) > 0:
			return nil, fmt.Errorf("agent %s: %w", r.agentID, ErrAgentEnded)
		case r.ended:
			continue
		case len(r.recent) >= domain.MaxAgentMessagesPerHour:
			return nil, &MessagesRateLimited{AgentID: r.agentID, Next: r.recent[len(r.recent)-domain.MaxAgentMessagesPerHour].Add(time.Hour).UTC()}
		}
		kept = append(kept, r)
	}
	for _, agentID := range to {
		if !found[agentID] {
			return nil, fmt.Errorf("agent %s: %w", agentID, ErrNotSignedUp)
		}
	}
	if len(kept) == 0 {
		return nil, ErrNoResearcherAtWork
	}

	from := domain.MessageFrom(sender)
	messages := make([]domain.AgentMessage, len(kept))
	for i, r := range kept {
		messages[i] = domain.AgentMessage{AgentID: r.agentID, From: from, Text: text, Actor: domain.Actor{Kind: sender.Kind, ID: sender.ID}}
		if err := tx.QueryRow(ctx, `INSERT INTO agent_messages (platform_experiment_id, agent_id, sender, text, actor_kind, actor_id, created_xid)
VALUES ($1, $2, $3, $4, $5, $6, pg_current_xact_id()::text::bigint) RETURNING created_at`,
			platformExpID, r.agentID, from, text, string(sender.Kind), sender.ID).Scan(&messages[i].At); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.SendAgentMessages: insert: %w", err)
		}
		messages[i].At = messages[i].At.UTC()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("platform_experiments_store.SendAgentMessages: commit: %w", err)
	}
	return messages, nil
}

// ListAgentMessages is every message sent in a platform experiment, to agentID alone when it is
// not empty, oldest first.
func (s *PlatformExperimentsStore) ListAgentMessages(ctx context.Context, platformExpID, agentID string) ([]domain.AgentMessage, error) {
	rows, err := s.pool.pool.Query(ctx, `SELECT agent_id, sender, text, actor_kind, actor_id, created_at FROM agent_messages
WHERE platform_experiment_id = $1 AND ($2 = '' OR agent_id = $2) ORDER BY id`, platformExpID, agentID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListAgentMessages: %w", err)
	}
	messages, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.AgentMessage, error) {
		var m domain.AgentMessage
		err := row.Scan(&m.AgentID, &m.From, &m.Text, &m.Actor.Kind, &m.Actor.ID, &m.At)
		m.At = m.At.UTC()
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.ListAgentMessages: %w", err)
	}
	return messages, nil
}
