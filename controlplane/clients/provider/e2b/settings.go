package e2b

import (
	"fmt"
	"time"
)

// Settings is the adapter's own section of the settings, under its name beside provider.
type Settings struct {
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
	if notice := int(expiryNotice / time.Second); c.LifetimeSeconds <= notice {
		return fmt.Errorf("e2b: lifetime_seconds must be above %d: a sandbox's command is ended that long before the sandbox, so its end can be read", notice)
	}
	return nil
}
