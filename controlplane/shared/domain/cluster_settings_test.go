package domain

import "testing"

func TestValidateClusterSettings(t *testing.T) {
	scaleUp := func(v int) *int { return &v }

	cases := []struct {
		name    string
		cs      *ClusterSettings
		wantErr bool
	}{
		{"nil fields ok", &ClusterSettings{ClusterID: "c1"}, false},
		{"valid timeout", &ClusterSettings{ClusterID: "c1", ScaleUpTimeoutSeconds: scaleUp(600)}, false},
		{"timeout at ceiling rejected", &ClusterSettings{ClusterID: "c1", ScaleUpTimeoutSeconds: scaleUp(1800)}, true},
		{"timeout above ceiling rejected", &ClusterSettings{ClusterID: "c1", ScaleUpTimeoutSeconds: scaleUp(3600)}, true},
		{"zero timeout rejected", &ClusterSettings{ClusterID: "c1", ScaleUpTimeoutSeconds: scaleUp(0)}, true},
		{"valid speculative cap", &ClusterSettings{ClusterID: "c1", MaxSpeculativeAccelerators: scaleUp(4)}, false},
		{"zero speculative cap rejected", &ClusterSettings{ClusterID: "c1", MaxSpeculativeAccelerators: scaleUp(0)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateClusterSettings(tc.cs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateClusterSettings(%+v): got err = %v, wantErr = %v", tc.cs, err, tc.wantErr)
			}
		})
	}
}
