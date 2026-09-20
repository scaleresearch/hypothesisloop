package main

import (
	"fmt"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/cloudexec"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/providers"
)

// newProvider builds the configured provider for the accelerator types of the shapes it will be
// asked to rent.
func newProvider(cfg providerConfig, shapes []cloudexec.Shape) (provider.Provider, error) {
	rented, err := providers.New(cfg.Name, cfg.Settings, acceleratorTypes(shapes))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return rented, nil
}

func acceleratorTypes(shapes []cloudexec.Shape) []string {
	var types []string
	for _, shape := range shapes {
		if shape.Accelerators > 0 {
			types = append(types, shape.AcceleratorType)
		}
	}
	return types
}
