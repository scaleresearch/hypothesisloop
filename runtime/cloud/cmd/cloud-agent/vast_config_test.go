package main

import (
	"os"
	"strings"
	"testing"
)

const shippedVastConfig = "../../settings/cloud-agent.vast.yaml"

func TestTheShippedVastConfigBuildsTheAdapter(t *testing.T) {
	t.Setenv("VAST_API_KEY", "not-a-real-key")
	cfg, err := loadConfig(shippedVastConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newProvider(cfg.Provider, cfg.shapes); err != nil {
		t.Fatalf("provider: %v", err)
	}
}

func TestAShapeTheVastSettingsCannotRentStopsStartup(t *testing.T) {
	t.Setenv("VAST_API_KEY", "not-a-real-key")
	raw, err := os.ReadFile(shippedVastConfig)
	if err != nil {
		t.Fatal(err)
	}
	const rented, unrentable = "accelerator_type: nvidia.com/gpu.product=NVIDIA-A100-80GB-PCIE", "accelerator_type: nvidia.com/gpu.product=NVIDIA-H200"
	if !strings.Contains(string(raw), rented) {
		t.Fatalf("the shipped config has no %q", rented)
	}
	cfg, err := loadText(t, strings.Replace(string(raw), rented, unrentable, 1))
	if err != nil {
		t.Fatal(err)
	}

	_, err = newProvider(cfg.Provider, cfg.shapes)
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-H200") {
		t.Fatalf("err = %v, want one naming the accelerator type the settings have no entry for", err)
	}
}
