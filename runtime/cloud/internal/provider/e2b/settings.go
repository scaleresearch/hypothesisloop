package e2b

import (
	"errors"
	"fmt"
	"os"
)

// Settings is the adapter's own section of the config file, provider.settings.
type Settings struct {
	// APIKeyEnv names the environment variable holding the team's API key.
	APIKeyEnv string `yaml:"api_key_env"`
	// LifetimeSeconds is how long a sandbox lives before the vendor kills it. Nothing extends
	// it: a sandbox that reaches it leaves the list and its caller creates it again. The plan
	// caps it, and a value above the cap fails every Create with the vendor's own message.
	LifetimeSeconds int `yaml:"lifetime_seconds"`
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
