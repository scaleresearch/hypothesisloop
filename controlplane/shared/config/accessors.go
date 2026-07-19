package config

// RateFor returns the AccH rate for the named accelerator type (e.g. "H100").
// Returns 1.0 (T4 base rate) for unknown types.
func (c *Config) RateFor(acceleratorName string) float64 {
	if r, ok := c.RateByName[acceleratorName]; ok {
		return r
	}
	return 1.0
}

// AcceleratorNameForFlavor returns the accelerator type name for a flavor name.
// Returns "" if not found.
func (c *Config) AcceleratorNameForFlavor(flavor string) string {
	return c.NameByFlavor[flavor]
}

// FlavorOrder returns flavor names in definition order.
func (c *Config) FlavorOrder() []string {
	out := make([]string, len(c.AcceleratorTypes))
	for i, g := range c.AcceleratorTypes {
		out[i] = g.Flavor
	}
	return out
}
