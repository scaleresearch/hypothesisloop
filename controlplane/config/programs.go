package config

import "fmt"

// ControlService is what control-service runs with: the settings and the secrets they call for.
type ControlService struct {
	*Settings
	DatabaseURL, MetricsDBURL, GraphDBURL, SigningSecret string
	ModelToken, DataStoreSecret                          string
	// GitHubKey is set on a github git host, and GitPassword on a plain one with a username.
	GitHubKey, GitPassword string
	// SandboxSecret is the sandbox provider's key, empty with no sandbox.
	SandboxSecret string
	// CloudComputeSecrets is the provider's key of each cloud_compute entry, by cluster name.
	CloudComputeSecrets map[string]string
}

func LoadControlService() (*ControlService, error) {
	s, env, err := load(ProgramControlService)
	if err != nil {
		return nil, err
	}
	c := &ControlService{
		Settings: s, DatabaseURL: env[DatabaseURL], MetricsDBURL: env[MetricsDBURL], GraphDBURL: env[GraphDBURL], SigningSecret: env[AuthSigningSecret],
		ModelToken: env[modelKeys[s.Models.Provider]], DataStoreSecret: env[dataStoreKeys[s.DataStore.Provider]],
		GitHubKey: env[GitHostGitHubAppPrivateKey], GitPassword: env[GitHostPlainPassword],
		CloudComputeSecrets: map[string]string{},
	}
	if s.Sandbox != nil {
		c.SandboxSecret = env[sandboxKey(s.Sandbox.Provider.Name)]
	}
	for _, cc := range s.CloudCompute {
		c.CloudComputeSecrets[cc.ClusterName] = env[cloudComputeKey(cc.Provider.Name)]
	}
	return c, nil
}

// MetricsService is what metrics-service runs with.
type MetricsService struct {
	*Settings
	DatabaseURL, MetricsDBURL, SigningSecret string
}

func LoadMetricsService() (*MetricsService, error) {
	s, env, err := load(ProgramMetricsService)
	if err != nil {
		return nil, err
	}
	return &MetricsService{Settings: s, DatabaseURL: env[DatabaseURL], MetricsDBURL: env[MetricsDBURL], SigningSecret: env[AuthSigningSecret]}, nil
}

// Admin is what hl-admin runs with. GitHubKey is set on a github git host.
type Admin struct {
	*Settings
	DatabaseURL, SigningSecret, GitHubKey string
}

func LoadAdmin() (*Admin, error) {
	s, env, err := load(ProgramAdmin)
	if err != nil {
		return nil, err
	}
	return &Admin{Settings: s, DatabaseURL: env[DatabaseURL], SigningSecret: env[AuthSigningSecret], GitHubKey: env[GitHostGitHubAppPrivateKey]}, nil
}

// E2BTemplate is what e2b-template runs with. It loads no settings. The registry login is for an
// image in a private registry.
type E2BTemplate struct {
	APIKey, RegistryUsername, RegistryPassword string
}

func LoadE2BTemplate() (*E2BTemplate, error) {
	const program = "e2b-template"
	env, err := read(program, SandboxE2BAPIKey)
	if err != nil {
		return nil, err
	}
	t := &E2BTemplate{APIKey: env[SandboxE2BAPIKey]}
	if lookup(SandboxE2BRegistryUsername) == "" && lookup(SandboxE2BRegistryPassword) == "" {
		return t, nil
	}
	login, err := read(program, SandboxE2BRegistryUsername, SandboxE2BRegistryPassword)
	if err != nil {
		return nil, fmt.Errorf("%w; the registry login is set whole or not at all", err)
	}
	t.RegistryUsername, t.RegistryPassword = login[SandboxE2BRegistryUsername], login[SandboxE2BRegistryPassword]
	return t, nil
}
