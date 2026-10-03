package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/agentlaunch"
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
		Name:          "main",
		APIURL:        "https://api.example.invalid",
		Command:       []string{"uv", "run", "python3", "-m", "hypothesisloop_agent.claude.run"},
		Size:          agentlaunch.Size{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 10 << 30},
		ModelProvider: "anthropic",
		Env:           map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth-secret-value"},
		Interval:      15 * time.Second,
		CreatePause:   time.Second,
	}
	if got := cfg.launch(); !reflect.DeepEqual(got, want) {
		t.Fatalf("launch config = %+v\nwant          %+v", got, want)
	}
	if cfg.Provider.Name != "example" {
		t.Fatalf("provider = %q", cfg.Provider.Name)
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
		"no interval":             {"reconcile_interval_seconds: 15\n", ""},
		"negative interval":       {"reconcile_interval_seconds: 15", "reconcile_interval_seconds: -1"},
		"a boot limit of its own": {"create_pause_milliseconds: 1000", "create_pause_milliseconds: 1000\nboot_limit_seconds: 120"},
		"no create pause":         {"create_pause_milliseconds: 1000\n", ""},
		"negative create pause":   {"create_pause_milliseconds: 1000", "create_pause_milliseconds: -1"},
		"no provider name":        {"name: example", `name: ""`},
		"misspelled in provider":  {"settings: {}", "setings: {}"},
		"no command":              {`command: ["uv", "run", "python3", "-m", "hypothesisloop_agent.claude.run"]`, "command: []"},
		"command not a list":      {`command: ["uv", "run", "python3", "-m", "hypothesisloop_agent.claude.run"]`, "command: uv run"},
		"bad quantity":            {"memory: 4Gi", "memory: lots"},
		"no cpu":                  {`cpu: "2"`, `cpu: "0"`},
		"no storage":              {"storage: 10Gi\n", ""},
		"misspelled in instance":  {"storage: 10Gi", "disk: 20Gi"},
		"a lifetime of its own":   {"storage: 10Gi", "storage: 10Gi\n  lifetime_seconds: 259200"},
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

func TestACreatePauseOfZeroIsNoPause(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret-value")
	cfg, err := loadText(t, strings.Replace(shipped(t), "create_pause_milliseconds: 1000", "create_pause_milliseconds: 0", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.launch().CreatePause; got != 0 {
		t.Fatalf("pause = %s", got)
	}
}
