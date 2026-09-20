package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AgentSignup is one agent's place in one platform experiment, and for an agent the platform
// runs, everything its machine is started with. All of it is fixed when the agent joins.
type AgentSignup struct {
	PlatformExperimentID string     `json:"platform_experiment_id"`
	AgentID              string     `json:"agent_id"`
	Name                 string     `json:"name"`
	Role                 SignupRole `json:"role"`
	QuotaTier            QuotaTier  `json:"quota_tier,omitempty"`
	// Hosted means the platform runs the agent: its launcher keeps one machine for it while the
	// platform experiment is open or running and the agent is not cut.
	Hosted bool `json:"hosted"`
	// Launcher is the launcher that runs a hosted agent, written when it joins so that a later
	// change of the configured launcher moves new agents only.
	Launcher        string         `json:"launcher,omitempty"`
	Flavor          string         `json:"flavor,omitempty"`
	Hyperparameters map[string]any `json:"hyperparameters"`
	Model           string         `json:"model,omitempty"`
	SignedUpAt      time.Time      `json:"signed_up_at"`
}

// MaxAgentsPerSpawn bounds how many agents one request may create.
const MaxAgentsPerSpawn = 100

const maxModelLength = 128

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]*$`)

// ValidateAgentLaunch checks what an agent is started with. Each value becomes an environment
// variable of the agent's machine, and an empty one leaves the harness's own default in place.
func ValidateAgentLaunch(flavor, model string) error {
	if flavor != "" {
		if err := ValidateIdentifier("flavor", flavor); err != nil {
			return err
		}
	}
	if model != "" && (len(model) > maxModelLength || !modelPattern.MatchString(model)) {
		return fmt.Errorf("model %q must be at most %d characters of letters, digits and . _ : - [ ]", model, maxModelLength)
	}
	return nil
}

// NewAgentID returns the id of an agent the platform creates: time-ordered, and within the
// identifier rule every agent id meets.
func NewAgentID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", errors.New("generate agent id: " + err.Error())
	}
	return "ag-" + strings.ReplaceAll(id.String(), "-", ""), nil
}
