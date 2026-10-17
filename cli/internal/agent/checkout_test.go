package agent

import (
	"testing"
)

func TestAnImplementationBranchIsNamedByItsAgentAndItsHypothesis(t *testing.T) {
	for _, tc := range []struct{ agent, text, id, want string }{
		{"Researcher 1", "FP16 at batch 32", "0198c2d4-aaaa-7bbb-8ccc-000000a1b2c3", "hl/pe-1234/researcher-1/fp16-at-batch-32-a1b2c3"},
		{"Researcher 2", "  Warmup: 5% of steps, then cosine decay to zero over the whole remaining run", "hyp-0198c2d4aaaa7bbb8ccc000000d4e5f6",
			"hl/pe-1234/researcher-2/warmup-5-of-steps-then-cosine-decay-to-z-d4e5f6"},
		{"Ada", "?!", "hyp-0198c2d4aaaa7bbb8ccc000000d4e5f6", "hl/pe-1234/ada/d4e5f6"},
	} {
		got, err := implementationBranch("pe-1234", tc.agent, tc.text, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("implementationBranch(%q, %q, %q) = %q, %v, want %q", tc.agent, tc.text, tc.id, got, err, tc.want)
		}
	}
	if got, err := implementationBranch("pe-1234", "!!", "idea", "hyp-0198c2d4aaaa7bbb8ccc000000d4e5f6"); err == nil {
		t.Errorf("an agent name with no letter or digit: got %q, want an error", got)
	}
}
