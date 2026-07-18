package config

// RateFor returns the T4h rate for the named GPU type (e.g. "H100").
// Returns 1.0 (T4 base rate) for unknown types.
func (c *Config) RateFor(gpuName string) float64 {
	if r, ok := c.RateByName[gpuName]; ok {
		return r
	}
	return 1.0
}

// GPUNameForFlavor returns the GPU type name for a flavor name.
// Returns "" if not found.
func (c *Config) GPUNameForFlavor(flavor string) string {
	return c.NameByFlavor[flavor]
}

// FlavorOrder returns flavor names in definition order.
func (c *Config) FlavorOrder() []string {
	out := make([]string, len(c.GPUTypes))
	for i, g := range c.GPUTypes {
		out[i] = g.Flavor
	}
	return out
}
