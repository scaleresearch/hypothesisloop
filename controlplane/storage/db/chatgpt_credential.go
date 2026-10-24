package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// renewLimit bounds a renewal of the ChatGPT credential, which runs to its commit whether or not
// its caller waits: the token endpoint rotates the refresh token as it answers, so a renewal it
// answered and the store did not keep leaves no refresh token that renews.
const renewLimit = time.Minute

// SeedChatGPTCredential stores c, read from the auth.json of digest, unless an auth.json of that
// digest was seeded before: the row then holds its renewals or a newer seed, which a restart keeps.
func (s *Store) SeedChatGPTCredential(ctx context.Context, c domain.ChatGPTCredential, digest string) error {
	_, err := s.pool.pool.Exec(ctx, `
INSERT INTO chatgpt_credential (access_token, refresh_token, account_id, expires_at, seed_digests) VALUES ($1, $2, $3, $4, ARRAY[$5::text])
ON CONFLICT (id) DO UPDATE SET access_token = EXCLUDED.access_token, refresh_token = EXCLUDED.refresh_token,
    account_id = EXCLUDED.account_id, expires_at = EXCLUDED.expires_at,
    seed_digests = chatgpt_credential.seed_digests || EXCLUDED.seed_digests, renewed_at = now()
WHERE NOT $5::text = ANY (chatgpt_credential.seed_digests)`, c.AccessToken, c.RefreshToken, c.AccountID, c.ExpiresAt, digest)
	if err != nil {
		return fmt.Errorf("db.SeedChatGPTCredential: %w", err)
	}
	return nil
}

// FreshChatGPTCredential is the stored ChatGPT credential, renewed by renew and stored first when
// due says it is. A due one is read again under the row's lock, so that of every caller of every
// replica one renews it and the others read the renewed pair: a refresh token renews once.
func (s *Store) FreshChatGPTCredential(ctx context.Context, due func(domain.ChatGPTCredential) bool,
	renew func(context.Context, domain.ChatGPTCredential) (domain.ChatGPTCredential, error)) (domain.ChatGPTCredential, error) {
	c, err := readChatGPTCredential(ctx, s.pool.pool, "")
	if err != nil {
		return domain.ChatGPTCredential{}, fmt.Errorf("db.FreshChatGPTCredential: %w", err)
	}
	if !due(c) {
		return c, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renewLimit)
	defer cancel()
	err = s.write(ctx, func(tx *Write) error {
		if c, err = readChatGPTCredential(ctx, tx, " FOR UPDATE"); err != nil || !due(c) {
			return err
		}
		if c, err = renew(ctx, c); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE chatgpt_credential SET access_token = $1, refresh_token = $2, account_id = $3, expires_at = $4, renewed_at = now()`,
			c.AccessToken, c.RefreshToken, c.AccountID, c.ExpiresAt)
		return err
	})
	if err != nil {
		return domain.ChatGPTCredential{}, fmt.Errorf("db.FreshChatGPTCredential: %w", err)
	}
	return c, nil
}

// ExpireChatGPTCredential makes the stored credential due, when its access token is accessToken,
// one the ChatGPT backend refused: the next call renews it.
func (s *Store) ExpireChatGPTCredential(ctx context.Context, accessToken string) error {
	if _, err := s.pool.pool.Exec(ctx, `UPDATE chatgpt_credential SET expires_at = now() WHERE access_token = $1`, accessToken); err != nil {
		return fmt.Errorf("db.ExpireChatGPTCredential: %w", err)
	}
	return nil
}

func readChatGPTCredential(ctx context.Context, q queryer, lock string) (domain.ChatGPTCredential, error) {
	var c domain.ChatGPTCredential
	err := q.QueryRow(ctx, `SELECT access_token, refresh_token, account_id, expires_at FROM chatgpt_credential`+lock).
		Scan(&c.AccessToken, &c.RefreshToken, &c.AccountID, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errors.New("no ChatGPT credential is stored: MODEL_OPENAI_AUTH_JSON seeds it at the start")
	}
	return c, err
}
