package vast

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/acceleratorname"
)

// Settings is the adapter's own section of the cloud-agent config file, provider.settings.
type Settings struct {
	// APIKeyEnv names the environment variable holding the account's API key.
	APIKeyEnv string `yaml:"api_key_env"`

	MaxPricePerHour float64 `yaml:"max_price_per_hour"`
	MinReliability  float64 `yaml:"min_reliability"`
	// Regions are two-letter country codes. Empty allows any.
	Regions      []string `yaml:"regions"`
	VerifiedOnly bool     `yaml:"verified_only"`

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
	if c.APIKeyEnv == "" {
		return errors.New("vast: api_key_env is required")
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

// accelerator resolves a catalog string the way the catalog itself matches labels: ignoring case.
func (c Settings) accelerator(catalogType string) (Accelerator, error) {
	for _, accelerator := range c.Accelerators {
		if strings.EqualFold(acceleratorname.NVIDIAProductLabel(accelerator.Product), catalogType) {
			return accelerator, nil
		}
	}
	return Accelerator{}, fmt.Errorf("vast: accelerator type %q has no entry in accelerators", catalogType)
}

// Covers says whether a job asking for catalogType can be rented with these settings.
func (c Settings) Covers(catalogType string) error {
	_, err := c.accelerator(catalogType)
	return err
}

func (c Settings) apiKey() (string, error) {
	key := os.Getenv(c.APIKeyEnv)
	if key == "" {
		return "", fmt.Errorf("vast: environment variable %s is not set", c.APIKeyEnv)
	}
	return key, nil
}
