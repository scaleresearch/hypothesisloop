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
	// it. The plan caps it, and a value above the cap fails every Create with the vendor's own
	// message. It is above expiryNotice.
	LifetimeSeconds int `yaml:"lifetime_seconds"`
}

// expiryNotice is how long before the vendor kills a sandbox at the plan's lifetime its
// supervisor ends the command and writes that down. A sandbox the vendor killed is gone from the
// list and nothing says why, so for this long it is listed as exited and expired, which is the
// time a caller has to read that: twenty passes at the interval the launcher ships with.
const expiryNotice = 5 * time.Minute

// lifetimes is how long a sandbox for spec is rented, in the vendor's seconds, and for how long
// its supervisor lets the command run, zero for no end of the supervisor's own.
//
// A spec whose own lifetime fits the plan's is rented for that and the vendor keeps it: when it
// ends nobody wants the instance, and it leaves the list. Any other is rented for the plan's
// longest, and its supervisor ends the command expiryNotice before the vendor ends the sandbox.
func (c Settings) lifetimes(spec provider.Spec) (sandboxSeconds int, command time.Duration) {
	longest := time.Duration(c.LifetimeSeconds) * time.Second
	if spec.Lifetime > 0 && spec.Lifetime <= longest {
		return int(spec.Lifetime / time.Second), 0
	}
	return c.LifetimeSeconds, longest - expiryNotice
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
