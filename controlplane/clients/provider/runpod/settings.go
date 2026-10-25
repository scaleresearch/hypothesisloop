package runpod

import (
	"errors"
	"fmt"
	"slices"
)

// Clouds are the vendor's two tiers: its own datacenters (SECURE) and community hosts (COMMUNITY).
var clouds = []string{"SECURE", "COMMUNITY"}

// Settings is the adapter's own section of a cloud_compute entry, runpod.
type Settings struct {
	// AccountID is the account's user id, as the Runpod console shows it. With the provider's
	// name it is the cluster's id.
	AccountID       string  `yaml:"account_id"`
	MaxPricePerHour float64 `yaml:"max_price_per_hour"`
	// Cloud is the tier GPU pods are rented on, SECURE or COMMUNITY. CPU pods are SECURE only.
	Cloud string `yaml:"cloud"`
	// DataCenters are the vendor's data center ids pods are rented in. Empty allows any.
	DataCenters []string `yaml:"data_centers"`
	// CPUFlavors are the vendor's CPU flavor ids a pod without a GPU is rented as, the cheapest
	// that fits first. Empty allows any.
	CPUFlavors []string `yaml:"cpu_flavors"`

	// Accelerators are the GPU models this account may rent, by catalog string, each as the
	// catalog's entry for runpod states it (accelerator_types[].providers.runpod).
	Accelerators map[string]Accelerator `yaml:"-"`
}

type Accelerator struct {
	// GPUID is the vendor's own id of the model, as its GPU catalog lists it.
	GPUID string `yaml:"gpu_id"`
	// MaxPricePerGPUHour, when set, is the ceiling of a pod of this model in place of the
	// account's max_price_per_hour: this times the pod's GPUs.
	MaxPricePerGPUHour float64 `yaml:"max_price_per_gpu_hour"`
}

func (c Settings) validate() error {
	if c.AccountID == "" {
		return errors.New("runpod: account_id is required")
	}
	if c.MaxPricePerHour <= 0 {
		return errors.New("runpod: max_price_per_hour must be positive")
	}
	if !slices.Contains(clouds, c.Cloud) {
		return fmt.Errorf("runpod: cloud %q must be one of %v", c.Cloud, clouds)
	}
	for catalogType, accelerator := range c.Accelerators {
		if accelerator.GPUID == "" {
			return fmt.Errorf("runpod: accelerator type %s: gpu_id is required", catalogType)
		}
		if accelerator.MaxPricePerGPUHour < 0 {
			return fmt.Errorf("runpod: accelerator type %s: max_price_per_gpu_hour must not be negative", catalogType)
		}
	}
	return nil
}

// accelerator is the entry of a catalog string.
func (c Settings) accelerator(catalogType string) (Accelerator, error) {
	accelerator, ok := c.Accelerators[catalogType]
	if !ok {
		return Accelerator{}, fmt.Errorf("runpod: accelerator type %q has no providers.runpod entry in accelerator_types", catalogType)
	}
	return accelerator, nil
}

// maxPrice is the most a pod of count GPUs of accelerator may cost an hour, with the setting
// that says so: the model's own ceiling times count where the catalog gives one, and the
// account's max_price_per_hour otherwise.
func (c Settings) maxPrice(accelerator Accelerator, count int) (float64, string) {
	if accelerator.MaxPricePerGPUHour > 0 {
		return accelerator.MaxPricePerGPUHour * float64(count), "max_price_per_gpu_hour"
	}
	return c.MaxPricePerHour, "max_price_per_hour"
}

// allows says whether pods may be rented in the data center.
func (c Settings) allows(dataCenter string) bool {
	return len(c.DataCenters) == 0 || slices.Contains(c.DataCenters, dataCenter)
}
