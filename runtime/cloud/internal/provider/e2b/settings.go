package e2b

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// Settings is the adapter's own section of the config file, provider.settings.
type Settings struct {
	// APIKeyEnv names the environment variable holding the team's API key.
	APIKeyEnv string `yaml:"api_key_env"`
	// LifetimeSeconds is the longest a sandbox lives before the vendor kills it. Nothing extends
	// it: a sandbox that reaches it leaves the list and its caller creates it again. The plan
	// caps it, and a value above the cap fails every Create with the vendor's own message.
	LifetimeSeconds int `yaml:"lifetime_seconds"`
}

// lifetimeSeconds is what a sandbox for spec is created with: the spec's own lifetime, which the
// vendor then keeps, under the longest this account's plan gives. The vendor rents no sandbox
// without one.
func (c Settings) lifetimeSeconds(spec provider.Spec) int {
	if spec.Lifetime > 0 {
		return min(c.LifetimeSeconds, int(spec.Lifetime/time.Second))
	}
	return c.LifetimeSeconds
}

func (c Settings) validate() error {
	if c.APIKeyEnv == "" {
		return errors.New("e2b: api_key_env is required")
	}
	if c.LifetimeSeconds <= 0 {
		return errors.New("e2b: lifetime_seconds must be positive")
	}
	return nil
}

func (c Settings) apiKey() (string, error) {
	key := os.Getenv(c.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("e2b: environment variable %s is not set", c.APIKeyEnv)
	}
	return key, nil
}
