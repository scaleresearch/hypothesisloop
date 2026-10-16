package domain

import "testing"

func TestGraceLimitsResolve(t *testing.T) {
	limits := GraceLimits{DefaultShutdownSeconds: 5, MaxShutdownSeconds: 30, MaxCheckpointSeconds: 600}
	ptr := func(v int64) *int64 { return &v }
	cases := []struct {
		name                 string
		shutdown, checkpoint *int64
		wantShutdown         int64
		wantCheckpoint       *int64
	}{
		{name: "default", wantShutdown: 5},
		{name: "requested", shutdown: ptr(20), wantShutdown: 20},
		{name: "capped", shutdown: ptr(90), wantShutdown: 30},
		{name: "checkpoint kept", checkpoint: ptr(300), wantShutdown: 5, wantCheckpoint: ptr(300)},
		{name: "checkpoint capped", checkpoint: ptr(86400), wantShutdown: 5, wantCheckpoint: ptr(600)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := JobSpec{TerminationGracePeriodSeconds: tc.shutdown, CheckpointGraceSeconds: tc.checkpoint}
			got, err := limits.Resolve(job)
			if err != nil {
				t.Fatal(err)
			}
			if *got.TerminationGracePeriodSeconds != tc.wantShutdown {
				t.Fatalf("shutdown grace = %d, want %d", *got.TerminationGracePeriodSeconds, tc.wantShutdown)
			}
			if (got.CheckpointGraceSeconds == nil) != (tc.wantCheckpoint == nil) ||
				(tc.wantCheckpoint != nil && *got.CheckpointGraceSeconds != *tc.wantCheckpoint) {
				t.Fatalf("checkpoint grace = %v, want %v", got.CheckpointGraceSeconds, tc.wantCheckpoint)
			}
			if tc.shutdown != nil && *tc.shutdown != *job.TerminationGracePeriodSeconds {
				t.Fatal("Resolve rewrote the submitted job's grace in place")
			}
		})
	}
}

func TestGraceLimitsResolveRefusesUnsetLimits(t *testing.T) {
	if _, err := (GraceLimits{}).Resolve(JobSpec{}); err == nil {
		t.Fatal("zero limits resolved a grace")
	}
}
