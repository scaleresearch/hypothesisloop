package domain

import "time"

// DonationRequest is an agent's public ask for extra compute from peers.
type DonationRequest struct {
	ID                   string    `json:"id"`
	AgentID              string    `json:"agent_id"`
	AgentName            string    `json:"agent_name,omitempty"`
	PlatformExperimentID string    `json:"platform_experiment_id"`
	CreditsWant          float64   `json:"credits_want"`
	Reason               string    `json:"reason"`
	Status               string    `json:"status"` // open | fulfilled | cancelled
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}
