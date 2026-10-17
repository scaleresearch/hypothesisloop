package fly

import (
	"fmt"
	"regexp"
	"strings"
)

// Settings is the adapter's own section of the settings, under its name beside provider.
type Settings struct {
	// Org is the slug of the Fly organization the apps are created in.
	Org string `yaml:"org"`
	// AppPrefix starts the name of every app the adapter creates, one for each Machine (apps.go).
	AppPrefix string `yaml:"app_prefix"`
	// Region is the three-letter code of the region Machines are placed in.
	Region string `yaml:"region"`
	// CPUKind is "shared" or "performance".
	CPUKind string `yaml:"cpu_kind"`
}

const tokenScheme = "FlyV1 "

var (
	orgSlug    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	appPrefix  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	regionCode = regexp.MustCompile(`^[a-z]{3}$`)
)

func (c Settings) validate() error {
	if !orgSlug.MatchString(c.Org) {
		return fmt.Errorf("fly: org %q must be the slug of the Fly organization the apps are created in", c.Org)
	}
	if !appPrefix.MatchString(c.AppPrefix) {
		return fmt.Errorf("fly: app_prefix %q must be 1 to 40 lowercase letters, digits and dashes, starting with a letter", c.AppPrefix)
	}
	if !regionCode.MatchString(c.Region) {
		return fmt.Errorf("fly: region %q must be a three-letter region code such as ams", c.Region)
	}
	if _, known := guestRules[c.CPUKind]; !known {
		return fmt.Errorf("fly: cpu_kind %q must be shared or performance", c.CPUKind)
	}
	return nil
}

// checkToken refuses what is not a token as `fly tokens create org` prints it: the scheme "FlyV1"
// and the token. The adapter creates and deletes apps, so the token is for the organization.
func checkToken(token string) error {
	if !strings.HasPrefix(token, tokenScheme) {
		return fmt.Errorf("fly: the API token must be one as `fly tokens create org` prints it, starting with %q", tokenScheme)
	}
	return nil
}
