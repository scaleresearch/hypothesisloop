package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/fake"
)

const shippedConfig = "../../settings/cloud-agent.yaml"

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
	path := filepath.Join(t.TempDir(), "cloud-agent.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(path)
}

func TestTheShippedConfigLoadsIntoShapesTheExecutorAccepts(t *testing.T) {
	cfg, err := loadConfig(shippedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.shapes) != 2 || cfg.shapes[0].CPUMillis != 16000 || cfg.shapes[0].MemoryBytes != 64<<30 || cfg.shapes[1].Accelerators != 0 {
		t.Fatalf("shapes = %+v", cfg.shapes)
	}
	exec, err := cloudexec.New(cloudexec.Config{
		ClusterID: cfg.Provider.Name + "-" + cfg.Provider.AccountID,
		APIURL:    cfg.APIURL, JobImage: cfg.JobImage, Shapes: cfg.shapes, Provider: fake.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := exec.GetClusterID(context.Background()); id != "example-1" {
		t.Fatalf("cluster id = %q", id)
	}
}

func TestAConfigThatIsIncompleteOrMisspelledIsRefused(t *testing.T) {
	for name, edit := range map[string][2]string{
		"unknown key":         {"cluster_name: rented", "cluster_name: rented\nmetrics_url: http://x"},
		"no cluster name":     {"cluster_name: rented", "cluster_name: \"\""},
		"no account id":       {`account_id: "1"`, `account_id: ""`},
		"no job image":        {"job_image: registry.example.invalid/hypothesisloop-job:latest", "job_image: \"\""},
		"bad quantity":        {"memory: 64Gi", "memory: lots"},
		"misspelled in shape": {"max_instances: 2", "max_instance: 2"},
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

type exampleSettings struct {
	Region string `yaml:"region"`
}

func settingsNode(t *testing.T, text string) yaml.Node {
	t.Helper()
	var holder struct {
		Settings yaml.Node `yaml:"settings"`
	}
	if err := yaml.Unmarshal([]byte(text), &holder); err != nil {
		t.Fatal(err)
	}
	return holder.Settings
}

func TestAProviderIsBuiltFromItsOwnSectionOfTheConfig(t *testing.T) {
	var got exampleSettings
	providers["example"] = adapter(func(s exampleSettings) (*fake.Provider, error) {
		got = s
		return fake.New(), nil
	})
	defer delete(providers, "example")

	var rented provider.Provider
	rented, err := newProvider(providerConfig{Name: "example", Settings: settingsNode(t, "settings: {region: eu}")})
	if err != nil || rented == nil || got.Region != "eu" {
		t.Fatalf("provider %v, settings %+v, %v", rented, got, err)
	}
	if _, err := newProvider(providerConfig{Name: "example", Settings: settingsNode(t, "settings: {regoin: eu}")}); err == nil {
		t.Error("a misspelled provider setting was accepted")
	}
	if _, err := newProvider(providerConfig{Name: "example"}); err != nil {
		t.Errorf("a provider with no settings of its own: %v", err)
	}
	if _, err := newProvider(providerConfig{Name: "other"}); err == nil {
		t.Error("a provider this binary was not built with was accepted")
	}
}
