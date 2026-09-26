package domain

import "testing"

func TestValidateClusterSettings(t *testing.T) {
	n := func(v int) *int { return &v }
	for name, tc := range map[string]struct {
		cs      ClusterSettings
		wantErr bool
	}{
		"nothing named":                            {ClusterSettings{}, false},
		"a timeout under the ceiling":              {ClusterSettings{ScaleUpTimeoutSeconds: n(600)}, false},
		"a timeout at the ceiling":                 {ClusterSettings{ScaleUpTimeoutSeconds: n(1800)}, true},
		"0 puts a field back on the default":       {ClusterSettings{ScaleUpTimeoutSeconds: n(0), MaxSpeculativeAccelerators: n(0), StuckPendingTimeoutSeconds: n(0)}, false},
		"a speculative cap":                        {ClusterSettings{MaxSpeculativeAccelerators: n(4)}, false},
		"a stuck-pending timeout past the ceiling": {ClusterSettings{StuckPendingTimeoutSeconds: n(3600)}, false},
		"a negative timeout":                       {ClusterSettings{ScaleUpTimeoutSeconds: n(-1)}, true},
		"a negative cap":                           {ClusterSettings{MaxSpeculativeAccelerators: n(-1)}, true},
		"a negative stuck-pending timeout":         {ClusterSettings{StuckPendingTimeoutSeconds: n(-1)}, true},
	} {
		if err := ValidateClusterSettings(tc.cs); (err != nil) != tc.wantErr {
			t.Errorf("%s: got err = %v, wantErr = %v", name, err, tc.wantErr)
		}
	}
}
