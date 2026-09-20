package fly

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Settings is the adapter's own section of the config file, provider.settings.
type Settings struct {
	// APITokenEnv names the environment variable holding a deploy token for App, as
	// `fly tokens create deploy -a <app>` prints it: the scheme "FlyV1" and the token. The
	// adapter calls nothing above the app, and a token for the organization would let whoever
	// holds it into every other app there.
	APITokenEnv string `yaml:"api_token_env"`
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
	if c.APITokenEnv == "" {
		return errors.New("fly: api_token_env is required")
	}
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

func (c Settings) apiToken() (string, error) {
	token := os.Getenv(c.APITokenEnv)
	if token == "" {
		return "", fmt.Errorf("fly: environment variable %s is not set", c.APITokenEnv)
	}
	if !strings.HasPrefix(token, tokenScheme) {
		return "", fmt.Errorf("fly: environment variable %s must hold a token as `fly tokens create` prints it, starting with %q", c.APITokenEnv, tokenScheme)
	}
	return token, nil
}
