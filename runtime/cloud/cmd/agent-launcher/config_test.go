package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

const shippedConfig = "../../settings/agent-launcher.yaml"

func shipped(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(shippedConfig)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func loadText(t *testing.T, text string) (*config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-launcher.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(path)
}

func TestTheShippedConfigLoadsIntoWhatTheLauncherRuns(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret-value")
	cfg, err := loadConfig(shippedConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := agentlaunch.Config{
		Name:             "main",
		APIURL:           "https://api.example.invalid",
		Command:          []string{"/opt/hypothesisloop-agent/.venv/bin/python3", "-m", "hypothesisloop_agent.claude.run"},
		Size:             agentlaunch.Size{CPUMillis: 8000, MemoryBytes: 16 << 30, DiskBytes: 50 << 30},
		ModelProvider:    "anthropic",
		Env:              map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth-secret-value"},
		Interval:         2 * time.Second,
		CreatePause:      time.Second,
		ProgressInterval: 15 * time.Second,
	}
	if got := cfg.launch(); !reflect.DeepEqual(got, want) {
		t.Fatalf("launch config = %+v\nwant          %+v", got, want)
	}
	if cfg.Provider.Name != "example" {
		t.Fatalf("provider = %q", cfg.Provider.Name)
	}
	if got := cfg.warmPool(fake.New()); got != 2 {
		t.Fatalf("warm pool = %d, want the file's 2", got)
	}
}

func TestTheWarmPoolLeftOutIsTwoOnAProviderThatWarmsAndNoneOnAnother(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret-value")
	cfg, err := loadText(t, strings.Replace(shipped(t), "warm_pool_size: 2\n", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.warmPool(fake.New()); got != 2 {
		t.Errorf("on a provider that warms: %d", got)
	}
	if got := cfg.warmPool(struct{ provider.Provider }{fake.New()}); got != 0 {
		t.Errorf("on a provider that cannot warm: %d", got)
	}
	cfg, err = loadText(t, strings.Replace(shipped(t), "warm_pool_size: 2", "warm_pool_size: 0", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.warmPool(fake.New()); got != 0 {
		t.Errorf("set to 0: %d", got)
	}
}

func TestTheModelProvidersEnvironmentComesFromTheNamedVariablesAndIsRequired(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if _, err := loadConfig(shippedConfig); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("with the variable unset: err = %v, want one naming it", err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "the-default-variable")
	t.Setenv("TEAM_MODEL_KEY", "the-named-variable")
	t.Setenv("TEAM_MODEL_ORG", "the-second-variable")
	text := strings.Replace(shipped(t), "  name: anthropic\n  env:\n    - name: CLAUDE_CODE_OAUTH_TOKEN\n      value_env: CLAUDE_CODE_OAUTH_TOKEN",
		"  name: another\n  env:\n    - name: MODEL_API_KEY\n      value_env: TEAM_MODEL_KEY\n    - name: MODEL_ORG\n      value_env: TEAM_MODEL_ORG", 1)
	cfg, err := loadText(t, text)
	if err != nil {
		t.Fatal(err)
	}
	launch := cfg.launch()
	if launch.ModelProvider != "another" || !reflect.DeepEqual(launch.Env, map[string]string{"MODEL_API_KEY": "the-named-variable", "MODEL_ORG": "the-second-variable"}) {
		t.Fatalf("model provider %q with instance env %v", launch.ModelProvider, launch.Env)
	}
}

func TestAConfigThatIsIncompleteOrMisspelledIsRefused(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret-value")
	for name, edit := range map[string][2]string{
		"unknown key":             {"launcher_name: main", "launcher_name: main\nmetrics_url: http://x"},
		"no launcher name":        {"launcher_name: main", `launcher_name: ""`},
		"no api url":              {"api_url: https://api.example.invalid", `api_url: ""`},
		"a namespace":             {"launcher_name: main\n", "launcher_name: main\nnamespace: main\n"},
		"launcher name with dash": {"launcher_name: main", "launcher_name: team-a"},
		"no interval":             {"reconcile_interval_seconds: 2\n", ""},
		"negative interval":       {"reconcile_interval_seconds: 2", "reconcile_interval_seconds: -1"},
		"negative warm pool":      {"warm_pool_size: 2", "warm_pool_size: -1"},
		"a boot limit of its own": {"create_pause_milliseconds: 1000", "create_pause_milliseconds: 1000\nboot_limit_seconds: 120"},
		"no create pause":         {"create_pause_milliseconds: 1000\n", ""},
		"negative create pause":   {"create_pause_milliseconds: 1000", "create_pause_milliseconds: -1"},
		"no provider name":        {"name: example", `name: ""`},
		"misspelled in provider":  {"settings: {}", "setings: {}"},
		"no command":              {`command: ["/opt/hypothesisloop-agent/.venv/bin/python3", "-m", "hypothesisloop_agent.claude.run"]`, "command: []"},
		"command not a list":      {`command: ["/opt/hypothesisloop-agent/.venv/bin/python3", "-m", "hypothesisloop_agent.claude.run"]`, "command: python3"},
		"bad quantity":            {"memory: 16Gi", "memory: lots"},
		"no cpu":                  {`cpu: "8"`, `cpu: "0"`},
		"no storage":              {"storage: 50Gi\n", ""},
		"misspelled in instance":  {"storage: 50Gi", "disk: 20Gi"},
		"a lifetime of its own":   {"storage: 50Gi", "storage: 50Gi\n  lifetime_seconds: 259200"},
		"no model provider":       {"model_provider:\n  name: anthropic\n  env:\n    - name: CLAUDE_CODE_OAUTH_TOKEN\n      value_env: CLAUDE_CODE_OAUTH_TOKEN\n", ""},
		"no model provider name":  {"  name: anthropic", `  name: ""`},
		"no model provider env":   {"  env:\n    - name: CLAUDE_CODE_OAUTH_TOKEN\n      value_env: CLAUDE_CODE_OAUTH_TOKEN", "  env: []"},
		"the credential itself":   {"      value_env: CLAUDE_CODE_OAUTH_TOKEN", "      value: sk-ant-oat01-something"},
		"a value for a name":      {"      value_env: CLAUDE_CODE_OAUTH_TOKEN", "      value_env: sk-ant-oat01-something"},
		"a variable not a name":   {"    - name: CLAUDE_CODE_OAUTH_TOKEN", "    - name: claude token"},
		"one variable twice":      {"      value_env: CLAUDE_CODE_OAUTH_TOKEN", "      value_env: CLAUDE_CODE_OAUTH_TOKEN\n    - name: CLAUDE_CODE_OAUTH_TOKEN\n      value_env: CLAUDE_CODE_OAUTH_TOKEN"},
		"the old credential key":  {"model_provider:", "claude_code_oauth_token_env: CLAUDE_CODE_OAUTH_TOKEN\nmodel_provider:"},
	} {
		text := shipped(t)
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("%s: the shipped config has no %q", name, edit[0])
		}
		if _, err := loadText(t, strings.Replace(text, edit[0], edit[1], 1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
