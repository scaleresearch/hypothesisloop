package domain

import "time"

// Agent represents an autonomous research agent interacting with the platform.
type Agent struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	PerformanceScore float64   `json:"performance_score"`
	Top3Count        int       `json:"top3_count"` // number of top-3 placements ever
	CreatedAt        time.Time `json:"created_at"`
}
