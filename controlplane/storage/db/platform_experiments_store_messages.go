package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

var (
	// ErrAgentEnded is a message to an agent that finished or was cut: nothing would read it.
	ErrAgentEnded = errors.New("the agent's run has ended")
	// ErrNoResearcherAtWork is a message to every researcher when none is at work.
	ErrNoResearcherAtWork = errors.New("no researcher is at work")
	// ErrNoCoordinator is a message to the coordinator of a platform experiment the platform hosts
	// none for.
	ErrNoCoordinator = errors.New("the platform hosts no coordinator for it")
)

// MessagesRateLimited refuses a message to a recipient its sender already sent the most an hour
// allows.
type MessagesRateLimited struct {
	AgentID string
	// From is the sender whose messages of the hour are counted: coordinator or owner.
	From string
	// Next is when the oldest message of the hour leaves it, and the next may be sent.
	Next time.Time
}

func (e *MessagesRateLimited) Error() string {
	return fmt.Sprintf("agent %s was sent %d messages by the %s in the last hour", e.AgentID, domain.MaxAgentMessagesPerHour, e.From)
}

// SendAgentMessages stores text as a message from sender to the agent of to, or with to empty to
// every researcher at work, and returns them, or stores none: domain.ErrPlatformExperimentClosed once the
// platform experiment closed, domain.ErrNotSignedUp for an agent not in it, ErrAgentEnded for one that
// finished or was cut, ErrNoResearcherAtWork, and *MessagesRateLimited for an agent the same
// sender sent domain.MaxAgentMessagesPerHour in the last hour. To every researcher, those the
// sender may not message yet are returned as refused and the rest are sent, and only when none is
// is it *MessagesRateLimited, for the first of them. Each is told as agent.message.
func (s *Store) SendAgentMessages(ctx context.Context, platformExpID string, to []string, text string, sender domain.Actor) (sent []domain.AgentMessage, refused []MessagesRateLimited, err error) {
	var messages []domain.AgentMessage
	if err := s.write(ctx, func(tx *Write) error {
		var status string
		err = tx.QueryRow(ctx, `SELECT status::text FROM platform_experiments WHERE id = $1 FOR SHARE`, platformExpID).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return domain.ErrNotSignedUp
		case err != nil:
			return fmt.Errorf("platform_experiments_store.SendAgentMessages: %w", err)
		case status == string(domain.PlatformExpClosed):
			return domain.ErrPlatformExperimentClosed
		}

		if to == nil {
			to = []string{}
		}
		from := domain.MessageFrom(sender)
		// The signups are locked so that two sends to one agent count each other.
		rows, err := tx.Query(ctx, `
SELECT s.agent_id, s.finished_at IS NOT NULL OR EXISTS (
           SELECT 1 FROM platform_experiment_cuts c WHERE c.platform_experiment_id = s.platform_experiment_id AND c.agent_id = s.agent_id),
       (SELECT array_agg(m.created_at ORDER BY m.created_at) FROM agent_messages m
        WHERE m.platform_experiment_id = s.platform_experiment_id AND m.agent_id = s.agent_id AND m.sender = $5
          AND m.created_at > now() - interval '1 hour')
FROM experiment_signups s
WHERE s.platform_experiment_id = $1 AND s.role <> $2 AND (cardinality($3::text[]) = 0 AND s.role = $4 OR s.agent_id = ANY($3))
ORDER BY s.signed_up_at, s.agent_id
FOR UPDATE OF s`, platformExpID, string(domain.SignupRolePlanner), to, string(domain.SignupRoleCompetitor), from)
		if err != nil {
			return fmt.Errorf("platform_experiments_store.SendAgentMessages: %w", err)
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
			return fmt.Errorf("platform_experiments_store.SendAgentMessages: %w", err)
		}
		found := map[string]bool{}
		var kept []recipient
		for _, r := range recipients {
			found[r.agentID] = true
			switch {
			case r.ended && len(to) > 0:
				return fmt.Errorf("agent %s: %w", r.agentID, ErrAgentEnded)
			case r.ended:
				continue
			case len(r.recent) >= domain.MaxAgentMessagesPerHour:
				limited := MessagesRateLimited{AgentID: r.agentID, From: from, Next: r.recent[len(r.recent)-domain.MaxAgentMessagesPerHour].Add(time.Hour).UTC()}
				if len(to) > 0 {
					return &limited
				}
				refused = append(refused, limited)
				continue
			}
			kept = append(kept, r)
		}
		for _, agentID := range to {
			if !found[agentID] {
				return fmt.Errorf("agent %s: %w", agentID, domain.ErrNotSignedUp)
			}
		}
		if len(kept) == 0 && len(refused) > 0 {
			return &refused[0]
		}
		if len(kept) == 0 {
			return ErrNoResearcherAtWork
		}

		messages = make([]domain.AgentMessage, len(kept))
		for i, r := range kept {
			messages[i] = domain.AgentMessage{AgentID: r.agentID, From: from, Text: text, Actor: domain.Actor{Kind: sender.Kind, ID: sender.ID}}
			if err := tx.QueryRow(ctx, `INSERT INTO agent_messages (platform_experiment_id, agent_id, sender, text, actor_kind, actor_id)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
				platformExpID, r.agentID, from, text, string(sender.Kind), sender.ID).Scan(&messages[i].At); err != nil {
				return fmt.Errorf("platform_experiments_store.SendAgentMessages: insert: %w", err)
			}
			tx.agentMessage(platformExpID, r.agentID, from, text)
			messages[i].At = messages[i].At.UTC()
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return messages, refused, nil
}

// SendCoordinatorMessage stores text as a message from sender, a person or a key, to the hosted
// coordinator of a platform experiment, kept and told as agent.message under
// domain.CoordinatorRecipient, or stores none: domain.ErrPlatformExperimentClosed once it closed,
// ErrNoCoordinator where the platform hosts none, and *MessagesRateLimited past
// domain.MaxAgentMessagesPerHour of the sender's in the last hour.
func (s *Store) SendCoordinatorMessage(ctx context.Context, platformExpID, text string, sender domain.Actor) (*domain.AgentMessage, error) {
	var message *domain.AgentMessage
	if err := s.write(ctx, func(tx *Write) error {
		var err error
		from := domain.MessageFrom(sender)
		// The row is locked so that two sends to the coordinator count each other.
		var status string
		var hosted bool
		var recent []time.Time
		err = tx.QueryRow(ctx, `
SELECT p.status::text, p.coordinator_hosted,
       COALESCE((SELECT array_agg(m.created_at ORDER BY m.created_at) FROM agent_messages m
                 WHERE m.platform_experiment_id = p.id AND m.agent_id = $2 AND m.sender = $3
                   AND m.created_at > now() - interval '1 hour'), '{}')
FROM platform_experiments p WHERE p.id = $1 FOR NO KEY UPDATE OF p`, platformExpID, domain.CoordinatorRecipient, from).Scan(&status, &hosted, &recent)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNoCoordinator
		case err != nil:
			return fmt.Errorf("platform_experiments_store.SendCoordinatorMessage: %w", err)
		case status == string(domain.PlatformExpClosed):
			return domain.ErrPlatformExperimentClosed
		case !hosted:
			return ErrNoCoordinator
		case len(recent) >= domain.MaxAgentMessagesPerHour:
			return &MessagesRateLimited{AgentID: domain.CoordinatorRecipient, From: from, Next: recent[len(recent)-domain.MaxAgentMessagesPerHour].Add(time.Hour).UTC()}
		}
		message = &domain.AgentMessage{AgentID: domain.CoordinatorRecipient, From: from, Text: text, Actor: domain.Actor{Kind: sender.Kind, ID: sender.ID}}
		if err := tx.QueryRow(ctx, `INSERT INTO agent_messages (platform_experiment_id, agent_id, sender, text, actor_kind, actor_id)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING created_at`,
			platformExpID, domain.CoordinatorRecipient, from, text, string(sender.Kind), sender.ID).Scan(&message.At); err != nil {
			return fmt.Errorf("platform_experiments_store.SendCoordinatorMessage: insert: %w", err)
		}
		tx.agentMessage(platformExpID, domain.CoordinatorRecipient, from, text)
		message.At = message.At.UTC()
		return nil
	}); err != nil {
		return nil, err
	}
	return message, nil
}

// MessageAllowance is, for every agent of a platform experiment but its planner, how many more
// messages sender may send it now and when that next grows, counted as SendAgentMessages
// refuses: the messages of the other sender count apart.
func (s *Store) MessageAllowance(ctx context.Context, platformExpID string, sender domain.Actor) (map[string]domain.MessageAllowance, error) {
	rows, err := s.pool.pool.Query(ctx, `
SELECT s.agent_id, count(m.id), min(m.created_at)
FROM experiment_signups s
LEFT JOIN agent_messages m ON m.platform_experiment_id = s.platform_experiment_id AND m.agent_id = s.agent_id
                          AND m.sender = $3 AND m.created_at > now() - interval '1 hour'
WHERE s.platform_experiment_id = $1 AND s.role <> $2
GROUP BY s.agent_id`, platformExpID, string(domain.SignupRolePlanner), domain.MessageFrom(sender))
	if err != nil {
		return nil, fmt.Errorf("platform_experiments_store.MessageAllowance: %w", err)
	}
	defer rows.Close()
	out := map[string]domain.MessageAllowance{}
	for rows.Next() {
		var agentID string
		var sent int
		var oldest *time.Time
		if err := rows.Scan(&agentID, &sent, &oldest); err != nil {
			return nil, fmt.Errorf("platform_experiments_store.MessageAllowance: %w", err)
		}
		allowance := domain.MessageAllowance{LeftThisHour: max(0, domain.MaxAgentMessagesPerHour-sent)}
		if oldest != nil {
			next := oldest.Add(time.Hour).UTC()
			allowance.NextAt = &next
		}
		out[agentID] = allowance
	}
	return out, rows.Err()
}

// ListAgentMessages is every message sent in a platform experiment, to agentID alone when it is
// not empty, oldest first.
func (s *Store) ListAgentMessages(ctx context.Context, platformExpID, agentID string) ([]domain.AgentMessage, error) {
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

// agentMessage announces a message from from to agentID, with its text: the one change that
// carries what was said, since it is said to its reader alone.
func (w *Write) agentMessage(platformExpID, agentID, from, text string) {
	w.Change(domain.Change{Kind: domain.EventAgentMessage, Subject: agentID, Value: from, Detail: text,
		PlatformExperimentID: platformExpID, AgentID: agentID})
}
