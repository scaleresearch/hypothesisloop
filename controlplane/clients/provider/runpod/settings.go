package runpod

import (
	"errors"
	"fmt"
	"slices"

	"github.com/scaleresearch/hypothesisloop/contract/acceleratorname"
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

	// Accelerators are the GPU models this account may rent, each under both of its names.
	Accelerators []Accelerator `yaml:"accelerators"`
}

type Accelerator struct {
	// Product is the name the NVIDIA driver reports for the model ("NVIDIA GeForce RTX 4090").
	// The resource catalog string is derived from it the way every other runtime derives it.
	Product string `yaml:"product"`
	// GPUID is the vendor's own id of the model, as its GPU catalog lists it.
	GPUID string `yaml:"gpu_id"`
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
	for i, accelerator := range c.Accelerators {
		if accelerator.Product == "" || accelerator.GPUID == "" {
			return fmt.Errorf("runpod: accelerators[%d]: product and gpu_id are required", i)
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
	return Accelerator{}, fmt.Errorf("runpod: accelerator type %q has no entry in accelerators", catalogType)
}

// allows says whether pods may be rented in the data center.
func (c Settings) allows(dataCenter string) bool {
	return len(c.DataCenters) == 0 || slices.Contains(c.DataCenters, dataCenter)
}
