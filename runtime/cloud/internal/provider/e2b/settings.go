package e2b

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// Settings is the adapter's own section of the config file, provider.settings.
type Settings struct {
	// APIKeyEnv names the environment variable holding the team's API key.
	APIKeyEnv string `yaml:"api_key_env"`
	// LifetimeSeconds is the longest a sandbox lives before the vendor kills it. Nothing extends
	// it. The plan caps it, and a value above the cap fails every Create with the vendor's own
	// message. It is above expiryNotice.
	LifetimeSeconds int `yaml:"lifetime_seconds"`
}

// expiryNotice is how long before the vendor kills a sandbox at the plan's lifetime its
// supervisor ends the command and writes that down. A sandbox the vendor killed is gone from the
// list and nothing says why, so for this long it is listed as exited and expired, which is the
// time a caller has to read that: twenty passes at the interval the launcher ships with.
const expiryNotice = 5 * time.Minute

// commandLifetime is how long the supervisor lets a sandbox's command run: the plan's longest
// lifetime less expiryNotice, so the command is ended before the vendor ends the sandbox.
func (c Settings) commandLifetime() time.Duration {
	return time.Duration(c.LifetimeSeconds)*time.Second - expiryNotice
}

func (c Settings) validate() error {
	if c.APIKeyEnv == "" {
		return errors.New("e2b: api_key_env is required")
	}
	if notice := int(expiryNotice / time.Second); c.LifetimeSeconds <= notice {
		return fmt.Errorf("e2b: lifetime_seconds must be above %d: a sandbox's command is ended that long before the sandbox, so its end can be read", notice)
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
