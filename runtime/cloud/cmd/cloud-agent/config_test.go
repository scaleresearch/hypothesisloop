package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
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
	if len(cfg.shapes) != 1 || cfg.shapes[0].CPUMillis != 16000 || cfg.shapes[0].MemoryBytes != 64<<30 || cfg.shapes[0].Accelerators != 1 {
		t.Fatalf("shapes = %+v", cfg.shapes)
	}
	if cfg.ReconcileIntervalSeconds != 10 || cfg.StatusIntervalSeconds != 10 || cfg.LogTailLines != 100 || cfg.Namespace != "main" {
		t.Fatalf("config = %+v", cfg)
	}
	rented := fake.New()
	exec, err := cloudexec.New(cfg.executor(rented))
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := exec.GetClusterID(context.Background()); id != "example-1" {
		t.Fatalf("cluster id = %q", id)
	}
	// The namespace reaches the instance names the executor reads.
	if _, err := exec.ListManagedJobs(context.Background()); err != nil || rented.Calls("List") != 1 {
		t.Fatal(err)
	}
}

func TestAConfigThatIsIncompleteOrMisspelledIsRefused(t *testing.T) {
	for name, edit := range map[string][2]string{
		"unknown key":            {"cluster_name: rented", "cluster_name: rented\nmetrics_url: http://x"},
		"no cluster name":        {"cluster_name: rented", "cluster_name: \"\""},
		"no account id":          {`account_id: "1"`, `account_id: ""`},
		"no job image":           {"job_image: registry.example.invalid/hypothesisloop-job:latest", "job_image: \"\""},
		"bad quantity":           {"memory: 64Gi", "memory: lots"},
		"misspelled in shape":    {"max_instances: 2", "max_instance: 2"},
		"no api url":             {"api_url: https://api.example.invalid", "api_url: \"\""},
		"no namespace":           {"namespace: main\n", ""},
		"namespace with dash":    {"namespace: main", "namespace: team-a"},
		"no reconcile interval":  {"reconcile_interval_seconds: 10\n", ""},
		"zero status interval":   {"status_interval_seconds: 10", "status_interval_seconds: 0"},
		"no log tail":            {"log_tail_lines: 100\n", ""},
		"shape name with dash":   {"name: h100", "name: h100-big"},
		"type in another casing": {"nvidia.com/gpu.product=NVIDIA-H100", "nvidia.com/gpu.product=nvidia-h100"},
		"type not a label":       {"nvidia.com/gpu.product=NVIDIA-H100", "H100"},
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

func TestAProviderIsAskedForTheAcceleratorTypesOfTheShapesThatRentOne(t *testing.T) {
	shapes := []cloudexec.Shape{
		{Name: "h100", AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100", Accelerators: 1},
		{Name: "cpu"},
	}
	if got := acceleratorTypes(shapes); len(got) != 1 || got[0] != "nvidia.com/gpu.product=NVIDIA-H100" {
		t.Errorf("accelerator types = %q, want the one type a shape rents", got)
	}
}

func TestAProviderThisBinaryWasNotBuiltWithIsRefused(t *testing.T) {
	_, err := newProvider(providerConfig{Name: "other"}, nil)
	if err == nil || !strings.Contains(err.Error(), `config: provider.name "other"`) {
		t.Fatalf("err = %v, want one naming the config key and the provider", err)
	}
}

// The rented cluster's settings set every shape's max_instances far above what is asked for at
// once. The executor must still accept them: the instance index is part of a name of bounded
// length.
func TestTheVastConfigLoadsIntoShapesTheExecutorAccepts(t *testing.T) {
	cfg, err := loadConfig("../../settings/cloud-agent.vast.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range cfg.shapes {
		if shape.MaxInstances < 100 {
			t.Errorf("shape %s: max_instances = %d, want it far above what experiments ask for at once", shape.Name, shape.MaxInstances)
		}
	}
	if _, err := cloudexec.New(cfg.executor(fake.New())); err != nil {
		t.Fatal(err)
	}
}
