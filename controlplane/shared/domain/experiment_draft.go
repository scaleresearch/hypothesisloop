package domain

import (
	"encoding/json"
	"time"
)

// ExperimentDraftIDPrefix is the prefix of an experiment draft's id (NewID).
const ExperimentDraftIDPrefix = "draft"

// MaxExperimentDraftBytes bounds what one draft holds: the form as it was typed.
const MaxExperimentDraftBytes = 256 << 10

// ExperimentDraft is a new platform experiment's form as a person left it, kept in a project so
// that it is finished later. Nothing in it is checked: the create that finishes it checks all.
type ExperimentDraft struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"project_id"`
	Form      json.RawMessage `json:"form" doc:"The form as the client holds it, stored and returned unread."`
	Step      string          `json:"step" doc:"The step of the form the person was on."`
	Failure   string          `json:"failure" doc:"Why the last create from this draft was refused, empty when none was."`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}
