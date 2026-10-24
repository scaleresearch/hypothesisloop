package clusterapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSlotsTravelInRankOrderAndAJobWithoutThemReadsAsBefore(t *testing.T) {
	wire, err := json.Marshal(JobSpec{NumNodes: 2, Slots: []string{"h100-3", "h100-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"slots":["h100-3","h100-1"]`) {
		t.Fatalf("slots go as %s", wire)
	}
	var back JobSpec
	if err := json.Unmarshal(wire, &back); err != nil || len(back.Slots) != 2 || back.Slots[0] != "h100-3" {
		t.Fatalf("slots read back as %v, %v", back.Slots, err)
	}
	if wire, _ := json.Marshal(JobSpec{NumNodes: 1}); strings.Contains(string(wire), "slots") {
		t.Fatalf("a job with no slots names them: %s", wire)
	}
}
