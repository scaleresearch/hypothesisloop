package domain

import "fmt"

// ClusterSettings holds operator-set overrides for one cluster's autoscaler speculation
// behaviour, keyed by the runtime-derived cluster_id (kube-system namespace UID / machine-id) —
// never by cluster_name, so a rename or a duplicate display name can't split or merge them. A
// row exists only once an operator has set at least one value; a cluster with no row uses the
// global scheduler defaults.
type ClusterSettings struct {
	ClusterID string `json:"cluster_id"`
	// ScaleUpTimeoutSeconds overrides scheduler.scale_up_timeout_seconds for this cluster. Nil
	// defers to the global default. Must be < MaxScaleUpTimeoutSeconds when set — see
	// ValidateClusterSettings.
	ScaleUpTimeoutSeconds *int `json:"scale_up_timeout_seconds,omitempty"`
	// MaxSpeculativeAccelerators caps how many accelerators this cluster may hold in outstanding
	// speculative (SUBMITTED, capacity not yet live) footprint at once. Nil means no per-cluster
	// cap — only the platform experiment's max_concurrent_accelerators bounds it.
	MaxSpeculativeAccelerators *int `json:"max_speculative_accelerators,omitempty"`
}

// MaxScaleUpTimeoutSeconds is the hard ceiling ScaleUpTimeoutSeconds (and the global
// scheduler.scale_up_timeout_seconds default) must stay under: PyTorch's default rendezvous
// store timeout is 30 minutes, and a scale-up wait past that turns a scheduling failure into a
// real job failure at the landed ranks. Mirrors config.MaxScaleUpTimeoutSeconds — domain cannot
// import config (config imports domain), so the constant is duplicated here as the one other
// place the invariant must hold; config/load.go's build() and this validator are the two call
// sites and both are covered by tests asserting the same threshold.
const MaxScaleUpTimeoutSeconds = 1800

// ValidateClusterSettings enforces the same scale_up_timeout invariant the global config does,
// so a PUT can't put a cluster into a state that would fail a job's landed ranks at rendezvous.
func ValidateClusterSettings(cs *ClusterSettings) error {
	if cs.ScaleUpTimeoutSeconds != nil && (*cs.ScaleUpTimeoutSeconds <= 0 || *cs.ScaleUpTimeoutSeconds >= MaxScaleUpTimeoutSeconds) {
		return fmt.Errorf("scale_up_timeout_seconds must be positive and less than %d", MaxScaleUpTimeoutSeconds)
	}
	if cs.MaxSpeculativeAccelerators != nil && *cs.MaxSpeculativeAccelerators <= 0 {
		return fmt.Errorf("max_speculative_accelerators must be positive")
	}
	return nil
}
