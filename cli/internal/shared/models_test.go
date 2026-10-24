package shared

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func modelsServer(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{
			"models": []map[string]any{
				{"model": "anthropic/claude-sonnet-5", "provider": "anthropic", "name": "claude-sonnet-5", "input_usd_per_mtok": 3, "output_usd_per_mtok": 15},
				{"model": "openrouter/moonshotai/kimi-k2", "provider": "openrouter", "name": "MoonshotAI: Kimi K2", "context_length": 131072, "input_usd_per_mtok": 0.6, "output_usd_per_mtok": 2.5},
				{"model": "openrouter/openai/gpt-5-nano", "provider": "openrouter", "name": "OpenAI: GPT-5 Nano", "context_length": 400000, "input_usd_per_mtok": 0.05, "output_usd_per_mtok": 0.4},
			},
			"default_model": "anthropic/claude-sonnet-5", "coordinator_model": "anthropic/claude-opus-5-5",
		}
	})
}

func TestModelsPrintsATableAndTheDefaults(t *testing.T) {
	modelsServer(t)
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Models, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	lines := clitest.Lines(stdout.String())
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "MODEL") ||
		strings.Fields(lines[1])[0] != "anthropic/claude-sonnet-5" || strings.Fields(lines[1])[1] != "-" ||
		strings.Join(strings.Fields(lines[2]), " ") != "openrouter/moonshotai/kimi-k2 131072 0.60 2.50" ||
		lines[4] != "default: anthropic/claude-sonnet-5" || lines[5] != "coordinator: anthropic/claude-opus-5-5" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestModelsFiltersByProviderAndSearchInAnyCase(t *testing.T) {
	modelsServer(t)
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Models, []string{"--provider", "openrouter", "--search", "KIMI"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "kimi-k2") || strings.Contains(out, "gpt-5-nano") || strings.Contains(out, "claude-sonnet-5\t") {
		t.Fatalf("stdout = %q", out)
	}
	stdout.Reset()
	if code := clitest.Run(Models, []string{"--search", "gpt-5 nano"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "gpt-5-nano") {
		t.Fatalf("search by name: exit %d, stdout %q", code, stdout.String())
	}
}

func TestModelsJSONHoldsOnlyTheMatches(t *testing.T) {
	modelsServer(t)
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Models, []string{"--json", "--provider", "anthropic"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	var out modelListing
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Models) != 1 || out.Models[0].Model != "anthropic/claude-sonnet-5" || out.CoordinatorModel != "anthropic/claude-opus-5-5" {
		t.Fatalf("stdout = %q, err %v", stdout.String(), err)
	}
}

func TestModelsRefusesAProviderThePlatformDoesNotServe(t *testing.T) {
	modelsServer(t)
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Models, []string{"--provider", "openai"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "one of anthropic, openrouter, got openai") {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
}
