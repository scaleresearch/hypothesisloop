package provider

import (
	"strings"
	"testing"
)

func TestValidateNameHoldsEveryNameToOneAlphabetAndLength(t *testing.T) {
	for _, name := range []string{"hl-job-prod-exp-1-a0-h100-0", strings.Repeat("a", MaxNameLength)} {
		if err := ValidateName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "HL-job", "hl_job", "hl job", "hl.job", strings.Repeat("a", MaxNameLength+1)} {
		if err := ValidateName(name); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}
