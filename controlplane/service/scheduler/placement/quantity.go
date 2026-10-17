package placement

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// QuantityValue parses a JobSpec-style quantity string into the canonical unit
// domain.NodeResource* uses for key (millicores for CPU, bytes for memory/storage).
func QuantityValue(key, qty string) (float64, error) {
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		return 0, err
	}
	if key == domain.NodeResourceCPUMillicores {
		return float64(q.MilliValue()), nil
	}
	return float64(q.Value()), nil
}

// QuantityString renders a canonical-unit value (millicores for CPU, bytes for
// memory/storage) back into a JobSpec-style quantity string.
func QuantityString(key string, value float64) string {
	if key == domain.NodeResourceCPUMillicores {
		return resource.NewMilliQuantity(int64(value), resource.DecimalSI).String()
	}
	return resource.NewQuantity(int64(value), resource.BinarySI).String()
}
