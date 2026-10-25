package vast

import (
	"errors"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
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

	// Accelerators are the GPU models this account may rent, by catalog string, each as the
	// catalog's entry for vast states it (accelerator_types[].providers.vast).
	Accelerators map[string]Accelerator `yaml:"-"`
}

type Accelerator struct {
	// GPUName is the vendor's own model name, as its offer search returns it ("RTX 4090").
	GPUName string `yaml:"gpu_name"`
	// MinGPURAMMB tells apart models the vendor sells under one name (an A100 with 40 or 80 GB).
	MinGPURAMMB int `yaml:"min_gpu_ram_mb"`
	// MaxPricePerGPUHour, when set, is the ceiling of an instance of this model in place of the
	// account's max_price_per_hour: this times the instance's GPUs.
	MaxPricePerGPUHour float64 `yaml:"max_price_per_gpu_hour"`
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
	for catalogType, accelerator := range c.Accelerators {
		if accelerator.GPUName == "" {
			return fmt.Errorf("vast: accelerator type %s: gpu_name is required", catalogType)
		}
		if accelerator.MaxPricePerGPUHour < 0 {
			return fmt.Errorf("vast: accelerator type %s: max_price_per_gpu_hour must not be negative", catalogType)
		}
	}
	return nil
}

// accelerator is the entry of a catalog string.
func (c Settings) accelerator(catalogType string) (Accelerator, error) {
	accelerator, ok := c.Accelerators[catalogType]
	if !ok {
		return Accelerator{}, fmt.Errorf("vast: accelerator type %q has no providers.vast entry in accelerator_types", catalogType)
	}
	return accelerator, nil
}

// maxPrice is the most an instance of spec may cost an hour, with the setting that says so: its
// model's own ceiling times its GPUs where the catalog gives one, and the account's
// max_price_per_hour otherwise.
func (c Settings) maxPrice(spec provider.Spec) (float64, string) {
	if accelerator, ok := c.Accelerators[spec.AcceleratorType]; ok && spec.AcceleratorCount > 0 && accelerator.MaxPricePerGPUHour > 0 {
		return accelerator.MaxPricePerGPUHour * float64(spec.AcceleratorCount), "max_price_per_gpu_hour"
	}
	return c.MaxPricePerHour, "max_price_per_hour"
}
