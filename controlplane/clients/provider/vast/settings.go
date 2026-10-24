package vast

import (
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/contract/acceleratorname"
)

// Settings is the adapter's own section of a cloud_compute entry, vast.
type Settings struct {
	// AccountID is the account's user id, as the vast.ai console shows it. With the provider's
	// name it is the cluster's id.
	AccountID       string  `yaml:"account_id"`
	MaxPricePerHour float64 `yaml:"max_price_per_hour"`
	MinReliability  float64 `yaml:"min_reliability"`
	// Regions are two-letter country codes. Empty allows any.
	Regions      []string `yaml:"regions"`
	VerifiedOnly bool     `yaml:"verified_only"`
	// ExcludedMachines are vendor machine ids never rented, for a host seen failing to start
	// instances. The log names the machine of every instance rented and of every one stuck.
	ExcludedMachines []int64 `yaml:"excluded_machines"`
	// PrivateNetworks lets a multi-node job rent its ranks into one of the vendor's overlays.
	// Off, Networked offers none.
	PrivateNetworks bool `yaml:"private_networks"`

	// Accelerators are the GPU models this account may rent, each under both of its names.
	Accelerators []Accelerator `yaml:"accelerators"`
}

type Accelerator struct {
	// Product is the name the NVIDIA driver reports for the model ("NVIDIA GeForce RTX 4090").
	// The resource catalog string is derived from it the way every other runtime derives it.
	Product string `yaml:"product"`
	// GPUName is the vendor's own model name, as its offer search returns it ("RTX 4090").
	GPUName string `yaml:"gpu_name"`
	// MinGPURAMMB tells apart models the vendor sells under one name (an A100 with 40 or 80 GB).
	MinGPURAMMB int `yaml:"min_gpu_ram_mb"`
}

func (c Settings) validate() error {
	if c.AccountID == "" {
		return errors.New("vast: account_id is required")
	}
	if c.MaxPricePerHour <= 0 {
		return errors.New("vast: max_price_per_hour must be positive")
	}
	if c.MinReliability < 0 || c.MinReliability >= 1 {
		return fmt.Errorf("vast: min_reliability %v must be in [0, 1)", c.MinReliability)
	}
	for i, accelerator := range c.Accelerators {
		if accelerator.Product == "" || accelerator.GPUName == "" {
			return fmt.Errorf("vast: accelerators[%d]: product and gpu_name are required", i)
		}
	}
	return nil
}

// accelerator resolves a catalog string to the entry whose product it is made of.
func (c Settings) accelerator(catalogType string) (Accelerator, error) {
	for _, accelerator := range c.Accelerators {
		if acceleratorname.NVIDIAProductLabel(accelerator.Product) == catalogType {
			return accelerator, nil
		}
	}
	return Accelerator{}, fmt.Errorf("vast: accelerator type %q has no entry in accelerators", catalogType)
}
