package clusterapi

import "testing"

func TestResolvedGraceRefusesAnUnresolvedJob(t *testing.T) {
	if _, _, err := (JobSpec{}).ResolvedGrace(); err == nil {
		t.Fatal("a job without resolved termination grace was accepted")
	}
}
