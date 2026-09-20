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
		Name:      "main",
		APIURL:    "https://api.example.invalid",
		Namespace: "main",
		Command:   []string{"uv", "run", "python3", "-m", "hypothesisloop_agent.claude.run"},
		Size:      agentlaunch.Size{CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 20 << 30},
		Env:       map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth-secret-value"},
		Interval:  15 * time.Second,
	}
	if got := cfg.launch(); !reflect.DeepEqual(got, want) {
		t.Fatalf("launch config = %+v\nwant          %+v", got, want)
	}
	if cfg.Provider.Name != "example" {
		t.Fatalf("provider = %q", cfg.Provider.Name)
	}
}

func TestTheClaudeCredentialComesFromTheNamedVariableAndIsRequired(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if _, err := loadConfig(shippedConfig); err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("with the variable unset: err = %v, want one naming it", err)
	}

	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "the-default-variable")
	t.Setenv("TEAM_CLAUDE_TOKEN", "the-named-variable")
	text := strings.Replace(shipped(t), "claude_code_oauth_token_env: CLAUDE_CODE_OAUTH_TOKEN", "claude_code_oauth_token_env: TEAM_CLAUDE_TOKEN", 1)
	cfg, err := loadText(t, text)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.launch().Env; !reflect.DeepEqual(got, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "the-named-variable"}) {
		t.Fatalf("instance env added by the launcher = %v", got)
	}
}

func TestAConfigThatIsIncompleteOrMisspelledIsRefused(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-secret-value")
	for name, edit := range map[string][2]string{
		"unknown key":            {"launcher_name: main", "launcher_name: main\nmetrics_url: http://x"},
		"no launcher name":       {"launcher_name: main", `launcher_name: ""`},
		"no api url":             {"api_url: https://api.example.invalid", `api_url: ""`},
		"no namespace":           {"namespace: main\n", ""},
		"namespace with dash":    {"namespace: main", "namespace: team-a"},
		"no interval":            {"reconcile_interval_seconds: 15\n", ""},
		"negative interval":      {"reconcile_interval_seconds: 15", "reconcile_interval_seconds: -1"},
		"no provider name":       {"name: example", `name: ""`},
		"misspelled in provider": {"settings: {}", "setings: {}"},
		"no command":             {`command: ["uv", "run", "python3", "-m", "hypothesisloop_agent.claude.run"]`, "command: []"},
		"command not a list":     {`command: ["uv", "run", "python3", "-m", "hypothesisloop_agent.claude.run"]`, "command: uv run"},
		"bad quantity":           {"memory: 4Gi", "memory: lots"},
		"no cpu":                 {`cpu: "2"`, `cpu: "0"`},
		"no storage":             {"storage: 20Gi\n", ""},
		"misspelled in instance": {"storage: 20Gi", "disk: 20Gi"},
		"no credential variable": {"claude_code_oauth_token_env: CLAUDE_CODE_OAUTH_TOKEN", `claude_code_oauth_token_env: ""`},
		"the credential itself":  {"claude_code_oauth_token_env: CLAUDE_CODE_OAUTH_TOKEN", "claude_code_oauth_token: sk-something"},
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
