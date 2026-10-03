package main

import (
	"maps"
	"testing"
)

// A pair with no "=" was left out, so a node started without a label its jobs select it by.
func TestNodeLabelsThatAreNotKeyValueAreRefused(t *testing.T) {
	got, err := parseNodeLabels("zone=a, disk = nvme")
	if err != nil || !maps.Equal(got, map[string]string{"zone": "a", "disk": "nvme"}) {
		t.Fatalf("parseNodeLabels = %v (err %v), want both labels trimmed", got, err)
	}
	if got, err := parseNodeLabels(""); err != nil || len(got) != 0 {
		t.Fatalf("parseNodeLabels of nothing = %v (err %v), want no labels", got, err)
	}
	for _, raw := range []string{"zone=a,nvme", "=a", "zone=a,"} {
		if _, err := parseNodeLabels(raw); err == nil {
			t.Errorf("parseNodeLabels(%q) was accepted", raw)
		}
	}
}
