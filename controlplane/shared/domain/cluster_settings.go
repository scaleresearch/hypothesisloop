package domain

import "fmt"

// ClusterSettings are what the org that registered a cluster may set on it in place of the
// scheduler's own defaults. They matter for a cluster whose nodes appear when a job asks for
// them, by an autoscaler or by renting. A field that is nil uses the default.
type ClusterSettings struct {
	// ScaleUpTimeoutSeconds is how long a job placed ahead of capacity waits for the autoscaler
	// to bring a node, in place of scheduler.scale_up_timeout_seconds. Under
	// MaxScaleUpTimeoutSeconds.
	ScaleUpTimeoutSeconds *int `json:"scale_up_timeout_seconds,omitempty" doc:"How long a job placed ahead of capacity waits for this cluster's autoscaler to bring a node, under 1800. Absent, the platform's default."`
	// MaxSpeculativeAccelerators caps the accelerators of jobs placed on this cluster ahead of
	// capacity at once. Nil is no cap of the cluster's own: only the platform experiment's
	// max_concurrent_accelerators bounds it.
	MaxSpeculativeAccelerators *int `json:"max_speculative_accelerators,omitempty" doc:"The most accelerators that jobs placed on this cluster ahead of capacity hold at once: what its autoscaler can add. Absent, no cap."`
	// StuckPendingTimeoutSeconds is how long a job admitted here may stay unplaced before it is
	// evicted as stuck_pending, in place of scheduler.stuck_pending_timeout_seconds. A cluster
	// whose nodes are rented when a job arrives boots slowly and sets it longer.
	StuckPendingTimeoutSeconds *int `json:"stuck_pending_timeout_seconds,omitempty" doc:"How long a job admitted to this cluster may stay unplaced before it is evicted as stuck_pending. Absent, the platform's default."`
}

// MaxScaleUpTimeoutSeconds is the hard ceiling ScaleUpTimeoutSeconds (and the global
// scheduler.scale_up_timeout_seconds default) must stay under: PyTorch's default rendezvous
// store timeout is 30 minutes, and a scale-up wait past that turns a scheduling failure into a
// real job failure at the landed ranks. Mirrors config.MaxScaleUpTimeoutSeconds — domain cannot
// import config (config imports domain), so the constant is duplicated here as the one other
// place the invariant must hold; config/load.go's build() and this validator are the two call
// sites and both are covered by tests asserting the same threshold.
const MaxScaleUpTimeoutSeconds = 1800

// Empty reports whether no field is set.
func (cs *ClusterSettings) Empty() bool {
	return cs.ScaleUpTimeoutSeconds == nil && cs.MaxSpeculativeAccelerators == nil && cs.StuckPendingTimeoutSeconds == nil
}

// ValidateClusterSettings checks a change to a cluster's settings: each field it names is a
// value to set, or 0, which puts that field back on the default.
func ValidateClusterSettings(cs ClusterSettings) error {
	if cs.ScaleUpTimeoutSeconds != nil && (*cs.ScaleUpTimeoutSeconds < 0 || *cs.ScaleUpTimeoutSeconds >= MaxScaleUpTimeoutSeconds) {
		return fmt.Errorf("scale_up_timeout_seconds must be under %d, or 0 for the platform's default", MaxScaleUpTimeoutSeconds)
	}
	if cs.MaxSpeculativeAccelerators != nil && *cs.MaxSpeculativeAccelerators < 0 {
		return fmt.Errorf("max_speculative_accelerators must be positive, or 0 for no cap")
	}
	if cs.StuckPendingTimeoutSeconds != nil && *cs.StuckPendingTimeoutSeconds < 0 {
		return fmt.Errorf("stuck_pending_timeout_seconds must be positive, or 0 for the platform's default")
	}
	return nil
}
