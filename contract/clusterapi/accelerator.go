package clusterapi

import (
	"fmt"
	"strings"
)

// AcceleratorType identifies a piece of hardware by a fact its own driver publishes, written
// as a fully-qualified "key=value" pair — for example:
//
//	nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3   (NVIDIA GPU Feature Discovery node label)
//	amd.com/gpu.product-name=Instinct-MI300X       (AMD node labeller node label)
//	tenstorrent.com/chipArch=blackhole             (Tenstorrent DRA device attribute)
//
// The string is the driver's own output, verbatim. There is no platform-side name for hardware
// and no table translating one to the other: the same string an agent writes in a job spec is
// the string capacity reports, the string quota bills against, and the string recorded on the
// job's pods. That is the whole point — a mapping is a place where two names can disagree, so
// there isn't one.
//
// Being fully qualified is what makes this work without any per-vendor knowledge in the code.
// The key carries its vendor domain, so matching is a plain lookup against live inventory rather
// than a rule about which field a given vendor calls its model name. Runtime code contains no
// vendor/model enum, and the operator's catalog (hypothesisloop.yaml) only attaches a price to
// these strings — it never defines or renames them. Types compare without regard to case (Is).
type AcceleratorType string

// CPUType is the one type whose unit is a CPU core. A runtime offers its nodes' whole cores as
// units of it.
const CPUType AcceleratorType = "hypothesisloop.dev/cpu=core"

// TypeKind says what one unit of a type is: a device, or one CPU core.
type TypeKind string

const (
	TypeKindAccelerator TypeKind = "accelerator"
	TypeKindCPU         TypeKind = "cpu"
)

// Kind is a core for CPUType and a device for every other type.
func (g AcceleratorType) Kind() TypeKind {
	if g.Is(CPUType) {
		return TypeKindCPU
	}
	return TypeKindAccelerator
}

// Key is the label or attribute name before "=", and Value the driver-published value after it.
// Both are "" for a malformed type.
func (g AcceleratorType) Key() string {
	key, _, ok := g.split()
	if !ok {
		return ""
	}
	return key
}

func (g AcceleratorType) Value() string {
	_, value, ok := g.split()
	if !ok {
		return ""
	}
	return value
}

// Domain is the vendor domain of the key: "nvidia.com" for "nvidia.com/gpu.product".
func (g AcceleratorType) Domain() string {
	key := g.Key()
	slash := strings.Index(key, "/")
	if slash <= 0 {
		return ""
	}
	return key[:slash]
}

// Validate rejects anything that is not a fully-qualified driver-published key=value pair.
func (g AcceleratorType) Validate() error {
	key, value, ok := g.split()
	if !ok {
		return fmt.Errorf("accelerator type %q must be a driver-published \"key=value\" pair, e.g. nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", string(g))
	}
	if strings.Index(key, "/") <= 0 {
		return fmt.Errorf("accelerator type %q must have a vendor-qualified key (e.g. nvidia.com/gpu.product), so it can be traced to the driver that publishes it", string(g))
	}
	if value == "" {
		return fmt.Errorf("accelerator type %q has an empty value", string(g))
	}
	return nil
}

func (g AcceleratorType) split() (key, value string, ok bool) {
	s := string(g)
	eq := strings.Index(s, "=")
	if eq <= 0 || eq == len(s)-1 {
		return "", "", false
	}
	return s[:eq], s[eq+1:], true
}

// MatchesLabels reports whether a node publishing these labels carries this type. The value
// compares without regard to case, since discovery mechanisms case the same hardware
// differently; the key compares exact.
func (g AcceleratorType) MatchesLabels(labels map[string]string) bool {
	key, value, ok := g.split()
	if !ok {
		return false
	}
	return strings.EqualFold(labels[key], value)
}

// MatchesAttributes reports whether a DRA device of driver publishing these attributes, keyed by
// their bare names, carries this type.
func (g AcceleratorType) MatchesAttributes(driver string, attributes map[string]string) bool {
	key, value, ok := g.split()
	if !ok || g.Domain() != driver {
		return false
	}
	bare := key[strings.Index(key, "/")+1:]
	return strings.EqualFold(attributes[bare], value)
}

// Is reports whether g and other name the same type, without regard to case.
func (g AcceleratorType) Is(other AcceleratorType) bool {
	return strings.ToLower(string(g)) == strings.ToLower(string(other))
}
