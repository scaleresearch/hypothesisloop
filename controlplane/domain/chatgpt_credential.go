package domain

import "time"

// ChatGPTCredential is the ChatGPT OAuth tokens the platform calls openai models with, those of a
// Codex CLI signed in with ChatGPT. The access token expires at ExpiresAt, and the refresh token
// renews it once, rotating with every renewal.
type ChatGPTCredential struct {
	AccessToken, RefreshToken, AccountID string
	ExpiresAt                            time.Time
}
