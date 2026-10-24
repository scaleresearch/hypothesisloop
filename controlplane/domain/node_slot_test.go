package domain

import "testing"

func TestValidateSlotLabelsRefusesACountThatIsNoCountAndANearnessThatIsNoLevel(t *testing.T) {
	for name, tc := range map[string]struct {
		labels map[string]string
		ok     bool
	}{
		"slot with counts":       {map[string]string{NodeSlotLabel: "s", NodeSlotAcceleratorsLabel: "8", NodeSlotNetworkLabel: "0", NodeSlotNetworkWithinLabel: "zone"}, true},
		"slot without counts":    {map[string]string{NodeSlotLabel: "s"}, true},
		"node that is no slot":   {map[string]string{NodeSlotNetworkLabel: "x"}, true},
		"count that is no count": {map[string]string{NodeSlotLabel: "s", NodeSlotNetworkLabel: "two"}, false},
		"negative count":         {map[string]string{NodeSlotLabel: "s", NodeSlotAcceleratorsLabel: "-1"}, false},
		"empty count":            {map[string]string{NodeSlotLabel: "s", NodeSlotAcceleratorsLabel: ""}, false},
		"nearness no level":      {map[string]string{NodeSlotLabel: "s", NodeSlotNetworkWithinLabel: "rack"}, false},
	} {
		if err := ValidateSlotLabels(tc.labels); (err == nil) != tc.ok {
			t.Errorf("%s: ValidateSlotLabels = %v, want ok %v", name, err, tc.ok)
		}
	}
}
