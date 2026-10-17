package fly

import (
	"fmt"
	"regexp"
	"strings"
)

// Settings is the adapter's own section of the settings, under its name beside provider.
type Settings struct {
	// App is the one Fly app every Machine is created in. It exists before the adapter starts.
	App string `yaml:"app"`
	// Region is the three-letter code of the region Machines are placed in.
	Region string `yaml:"region"`
	// CPUKind is "shared" or "performance".
	CPUKind string `yaml:"cpu_kind"`
}

const tokenScheme = "FlyV1 "

var (
	appName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	regionCode = regexp.MustCompile(`^[a-z]{3}$`)
)

func (c Settings) validate() error {
	if !appName.MatchString(c.App) {
		return fmt.Errorf("fly: app %q must be the name of the Fly app that holds the machines", c.App)
	}
	if !regionCode.MatchString(c.Region) {
		return fmt.Errorf("fly: region %q must be a three-letter region code such as ams", c.Region)
	}
	if _, known := guestRules[c.CPUKind]; !known {
		return fmt.Errorf("fly: cpu_kind %q must be shared or performance", c.CPUKind)
	}
	return nil
}

// checkToken refuses what is not a token as `fly tokens create deploy -a <app>` prints it: the
// scheme "FlyV1" and the token. The adapter calls nothing above the app, and a token for the
// organization would let whoever holds it into every other app there.
func checkToken(token string) error {
	if !strings.HasPrefix(token, tokenScheme) {
		return fmt.Errorf("fly: the API token must be one as `fly tokens create deploy` prints it, starting with %q", tokenScheme)
	}
	return nil
}
