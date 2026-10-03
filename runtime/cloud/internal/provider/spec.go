package provider

import (
	"fmt"
	"regexp"
	"time"
)

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate is what every adapter asks of a spec before it spends anything: a name that meets
// ValidateName, an image, a command, sizes, and environment names that are names. Vendors take
// an environment as a map or as shell text, and a key that is no name is an option or a command
// there.
func (s Spec) Validate() error {
	if err := ValidateName(s.Name); err != nil {
		return err
	}
	if s.Image == "" {
		return fmt.Errorf("spec %q has no image", s.Name)
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		return fmt.Errorf("spec %q has no command", s.Name)
	}
	if s.AcceleratorCount < 0 {
		return fmt.Errorf("spec %q asks for %d accelerators", s.Name, s.AcceleratorCount)
	}
	if s.CPUMillis <= 0 || s.MemoryBytes <= 0 || s.DiskBytes <= 0 {
		return fmt.Errorf("spec %q must state cpu, memory and disk", s.Name)
	}
	if s.Lifetime < 0 || s.Lifetime%time.Second != 0 {
		return fmt.Errorf("spec %q has a lifetime of %s, and a lifetime is whole seconds", s.Name, s.Lifetime)
	}
	for name := range s.Env {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("spec %q: %q is not an environment variable name", s.Name, name)
		}
	}
	return nil
}

// CeilDiv is how a spec's size becomes a vendor's unit: rounded up, so an instance is never
// smaller than asked for.
func CeilDiv(value, unit int64) int64 {
	return (value + unit - 1) / unit
}
