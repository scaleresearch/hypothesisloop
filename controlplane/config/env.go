package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Key is one variable of the environment a program reads: where its settings file is, or a secret
// of the deployment. The same name is used in every file that sets it and in the process.
type Key struct {
	Name string
	// For is what the value is, said when a program refuses to start without it.
	For string
}

// The sections of the settings whose provider can be switched, as the names of their keys in the
// environment begin.
const (
	SectionObjectStore       = "OBJECT_STORE"
	SectionGitHost           = "GIT_HOST"
	SectionContainerRegistry = "CONTAINER_REGISTRY"
	SectionSandbox           = "SANDBOX"
	SectionCloudCompute      = "CLOUD_COMPUTE"
)

// ProviderKey is a secret field of a section: <SECTION>_<NAME>_<FIELD>. NAME is the provider of a
// section that has one, and the cluster_name of an entry of cloud_compute, which lists several
// that may share a provider, in capitals with each '-' an '_'.
func ProviderKey(section, name, field, purpose string) Key {
	return Key{section + "_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_" + field, purpose}
}

// AccountKey is the one secret the sandbox's provider or a cloud_compute entry takes, whatever the
// provider: the key of its account at the vendor, <SECTION>_<NAME>_API_KEY.
func AccountKey(section, name string) Key {
	return ProviderKey(section, name, "API_KEY", "the API key of the account at the vendor that "+strings.ToLower(section)+" "+name+" rents on")
}

var (
	SettingsPath                    = Key{"HYPOTHESISLOOP_CONFIG", "the path of the settings file"}
	DatabaseURL                     = Key{"DATABASE_URL", "the address of the relational database, its password in it"}
	MetricsDBURL                    = Key{"METRICS_DB_URL", "the address of the metrics store"}
	GraphDBURL                      = Key{"GRAPH_DB_URL", "the bolt address of the hypothesis graph"}
	AuthSigningSecret               = Key{"AUTH_SIGNING_SECRET", fmt.Sprintf("the secret every hls_ token is derived from, at least %d characters", MinSigningSecretLength)}
	ModelAnthropicOAuthToken        = Key{"MODEL_ANTHROPIC_OAUTH_TOKEN", "the Claude subscription token every model call of a sandboxed agent is made with"}
	ObjectStoreMinIOSecretAccessKey = ProviderKey(SectionObjectStore, ObjectStoreProviderMinIO, "SECRET_ACCESS_KEY", "the secret key of object_store.minio.access_key_id")
	GitHostGitHubAppPrivateKey      = ProviderKey(SectionGitHost, GitHostProviderGitHub, "APP_PRIVATE_KEY", "the PEM GitHub issued for the App of git_host.github")
	GitHostGitHubClientSecret       = ProviderKey(SectionGitHost, GitHostProviderGitHub, "CLIENT_SECRET", "the client secret of git_host.github.client_id")
	GitHostPlainPassword            = ProviderKey(SectionGitHost, GitHostProviderPlain, "PASSWORD", "the password of git_host.plain.username")
	SandboxE2BAPIKey                = AccountKey(SectionSandbox, "e2b")
)

// providerField is <PROVIDER>_<FIELD> of a provider's key, each of capitals and digits.
var providerField = regexp.MustCompile(`^[A-Z0-9]+_[A-Z0-9]+(_[A-Z0-9]+)*$`)

// IsKey reports whether name is a key some program reads: one of the platform's own, or a
// provider's, which begins with its section. The provider of the sandbox and a cloud_compute entry
// are any the settings name, so their keys are known by the rule alone.
func IsKey(name string) bool {
	for _, key := range []Key{SettingsPath, DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret} {
		if name == key.Name {
			return true
		}
	}
	for _, section := range []string{SectionObjectStore, SectionGitHost, SectionContainerRegistry, SectionSandbox, SectionCloudCompute, "MODEL"} {
		if rest, ok := strings.CutPrefix(name, section+"_"); ok && providerField.MatchString(rest) {
			return true
		}
	}
	return false
}

// The secret of each provider that has one, by the provider's name in the settings. A provider
// that is not listed takes none.
var (
	modelKeys       = map[string]Key{ModelProviderAnthropic: ModelAnthropicOAuthToken}
	objectStoreKeys = map[string]Key{ObjectStoreProviderMinIO: ObjectStoreMinIOSecretAccessKey}
)

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
		keys := []Key{DatabaseURL, MetricsDBURL, GraphDBURL, AuthSigningSecret, modelKeys[s.Models.Provider], objectStoreKeys[s.ObjectStore.Provider]}
		keys = append(keys, s.gitHostKeys()...)
		if s.Sandbox != nil {
			keys = append(keys, AccountKey(SectionSandbox, s.Sandbox.Provider))
		}
		for _, cc := range s.CloudCompute {
			keys = append(keys, AccountKey(SectionCloudCompute, cc.ClusterName))
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
		return []Key{GitHostGitHubAppPrivateKey, GitHostGitHubClientSecret}
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
