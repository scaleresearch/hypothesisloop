package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

// Key is one variable of the environment a program reads: where its settings file is, or a secret
// of the deployment. Its name is ROLE[_PROVIDER]_NAME, the same in every file that sets it and in
// the process.
type Key struct {
	Name string
	// For is what the value is, said when a program refuses to start without it.
	For string
}

var (
	SettingsPath                  = Key{"HYPOTHESISLOOP_CONFIG", "the path of the settings file"}
	DatabaseURL                   = Key{"DATABASE_URL", "the address of the relational database, its password in it"}
	MetricsDBURL                  = Key{"METRICS_DB_URL", "the address of the metrics store"}
	GraphDBURL                    = Key{"GRAPH_DB_URL", "the bolt address of the hypothesis graph"}
	AuthSigningSecret             = Key{"AUTH_SIGNING_SECRET", fmt.Sprintf("the secret every hls_ token is derived from, at least %d characters", MinSigningSecretLength)}
	ModelAnthropicOAuthToken      = Key{"MODEL_ANTHROPIC_OAUTH_TOKEN", "the Claude subscription token every model call of a sandboxed agent is made with"}
	DataStoreMinIOSecretAccessKey = Key{"DATA_STORE_MINIO_SECRET_ACCESS_KEY", "the secret key of data_store.access_key_id"}
	GitHostGitHubAppPrivateKey    = Key{"GIT_HOST_GITHUB_APP_PRIVATE_KEY", "the PEM GitHub issued for the App of git_host.github"}
	GitHostPlainPassword          = Key{"GIT_HOST_PLAIN_PASSWORD", "the password of git_host.plain.username"}
	SandboxE2BAPIKey              = sandboxKey("e2b")
	SandboxE2BRegistryUsername    = Key{"SANDBOX_E2B_REGISTRY_USERNAME", "the login to the private registry of the image a template is built from"}
	SandboxE2BRegistryPassword    = Key{"SANDBOX_E2B_REGISTRY_PASSWORD", "the password of SANDBOX_E2B_REGISTRY_USERNAME"}
)

// Keys is every key a program reads.
var Keys = []Key{
	SettingsPath, DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret, ModelAnthropicOAuthToken,
	DataStoreMinIOSecretAccessKey, GitHostGitHubAppPrivateKey, GitHostPlainPassword,
	SandboxE2BAPIKey, SandboxE2BRegistryUsername, SandboxE2BRegistryPassword,
}

// The key of each provider that has one, by the provider's name in the settings. A provider that
// is not listed takes none.
var (
	modelKeys     = map[string]Key{ModelProviderAnthropic: ModelAnthropicOAuthToken}
	dataStoreKeys = map[string]Key{DataStoreProviderMinIO: DataStoreMinIOSecretAccessKey}
)

// sandboxKey and cloudComputeKey are the account key of a rented provider, by its name in the
// settings: SANDBOX_<NAME>_API_KEY and CLOUD_COMPUTE_<NAME>_API_KEY.
func sandboxKey(provider string) Key {
	return Key{"SANDBOX_" + strings.ToUpper(provider) + "_API_KEY", "the API key of the " + provider + " account the sandbox rents machines on"}
}

func cloudComputeKey(provider string) Key {
	return Key{"CLOUD_COMPUTE_" + strings.ToUpper(provider) + "_API_KEY", "the API key of the " + provider + " account of the cloud_compute entries on " + provider}
}

// Program is a process that loads the settings file.
type Program string

const (
	ProgramControlService Program = "control-service"
	ProgramMetricsService Program = "metrics-service"
	ProgramAdmin          Program = "hl-admin"
)

// Programs is every program that loads the settings file.
var Programs = []Program{ProgramControlService, ProgramMetricsService, ProgramAdmin}

// Needs is the keys p requires under these settings, beside SettingsPath.
func (s *Settings) Needs(p Program) []Key {
	switch p {
	case ProgramControlService:
		keys := []Key{DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret, modelKeys[s.Models.Provider], dataStoreKeys[s.DataStore.Provider]}
		keys = append(keys, s.gitHostKeys()...)
		if s.Sandbox != nil {
			keys = append(keys, sandboxKey(s.Sandbox.Provider.Name))
		}
		for _, cc := range s.CloudCompute {
			if key := cloudComputeKey(cc.Provider.Name); !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		return keys
	case ProgramMetricsService:
		return []Key{DatabaseURL, MetricsDBURL, AuthSigningSecret}
	case ProgramAdmin:
		keys := []Key{DatabaseURL, AuthSigningSecret}
		if s.GitHost.Provider == GitHostProviderGitHub {
			keys = append(keys, GitHostGitHubAppPrivateKey)
		}
		return keys
	}
	panic(fmt.Sprintf("config: %q is not a program that loads the settings", p))
}

func (s *Settings) gitHostKeys() []Key {
	switch {
	case s.GitHost.Provider == GitHostProviderGitHub:
		return []Key{GitHostGitHubAppPrivateKey}
	case s.GitHost.Provider == GitHostProviderPlain && s.GitHost.Plain.Username != "":
		return []Key{GitHostPlainPassword}
	}
	return nil
}

// read is the value of each of keys in the environment. One unset or empty refuses the start of
// program, naming the key and what it is for.
func read(program string, keys ...Key) (map[Key]string, error) {
	values := make(map[Key]string, len(keys))
	for _, key := range keys {
		value := os.Getenv(key.Name)
		if value == "" {
			return nil, fmt.Errorf("%s: %s is required: %s", program, key.Name, key.For)
		}
		values[key] = value
	}
	if secret, read := values[AuthSigningSecret]; read && len(secret) < MinSigningSecretLength {
		return nil, fmt.Errorf("%s: %s is shorter than %d characters", program, AuthSigningSecret.Name, MinSigningSecretLength)
	}
	return values, nil
}

// load is the settings file SettingsPath names and the keys p needs under it.
func load(p Program) (*Settings, map[Key]string, error) {
	path, err := read(string(p), SettingsPath)
	if err != nil {
		return nil, nil, err
	}
	s, err := LoadSettings(path[SettingsPath])
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", p, err)
	}
	env, err := read(string(p), s.Needs(p)...)
	if err != nil {
		return nil, nil, err
	}
	return s, env, nil
}

// lookup is the value of an optional key, empty when it is unset.
func lookup(key Key) string {
	return os.Getenv(key.Name)
}
