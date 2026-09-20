package main

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/vast"
)

const shippedVastConfig = "../../settings/cloud-agent.vast.yaml"

func TestTheShippedVastConfigBuildsTheAdapterAndCoversItsShapes(t *testing.T) {
	t.Setenv("VAST_API_KEY", "not-a-real-key")
	cfg, err := loadConfig(shippedVastConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newProvider(cfg.Provider); err != nil {
		t.Fatalf("provider: %v", err)
	}
	var settings vast.Settings
	if err := cfg.Provider.Settings.Decode(&settings); err != nil {
		t.Fatal(err)
	}
	for _, shape := range cfg.shapes {
		if shape.Accelerators == 0 {
			continue
		}
		if err := settings.Covers(shape.AcceleratorType); err != nil {
			t.Errorf("shape %s: %v", shape.Name, err)
		}
	}
}

func TestAVastConfigWithoutItsKeyIsRefused(t *testing.T) {
	t.Setenv("VAST_API_KEY", "")
	cfg, err := loadConfig(shippedVastConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newProvider(cfg.Provider); err == nil {
		t.Fatal("a provider was built with no API key in the environment")
	}
}
