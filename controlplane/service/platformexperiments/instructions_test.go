package platformexperiments

import (
	"context"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// A new version of the platform's rules is a new digest, so every harness applies it.
func TestTheRulesVersionIsPartOfTheDigest(t *testing.T) {
	src, _ := platformexperimentstest.PinnedInstructions{RulesVersion: 1}.AgentInstructions(context.Background(), "pe-1", "ag-1")
	one, err := composeInstructions("pe-1", "ag-1", src, platformexperimentstest.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	src, _ = platformexperimentstest.PinnedInstructions{RulesVersion: 2}.AgentInstructions(context.Background(), "pe-1", "ag-1")
	two, err := composeInstructions("pe-1", "ag-1", src, platformexperimentstest.Defaults)
	if err != nil || one.Digest == two.Digest || two.RulesVersion != 2 {
		t.Errorf("rules 1 and 2: %s and %+v, %v, want two digests", one.Digest, two, err)
	}
}

type pinnedExperiments struct {
	Store
	pinned platformexperimentstest.PinnedInstructions
}

func (p pinnedExperiments) AgentInstructions(ctx context.Context, pe, agent string) (*db.InstructionsSource, error) {
	return p.pinned.AgentInstructions(ctx, pe, agent)
}

// The roster says whether the instructions a harness runs are the pinned ones by the digest it
// prints, the start of the platform's own.
func TestTheRosterSaysWhetherAHarnessRunsThePinnedInstructions(t *testing.T) {
	src, _ := platformexperimentstest.PinnedInstructions{}.AgentInstructions(context.Background(), "pe-1", "ag-1")
	pinned, err := composeInstructions("pe-1", "ag-1", src, domain.ModelDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	svc := &Service{store: pinnedExperiments{}}
	for _, c := range []struct {
		name, agent, digest string
		want                *bool
	}{
		{"the pinned instructions", "ag-1", pinned.Digest[:domain.InstructionsDigestLength], &yes},
		{"an older version", "ag-1", strings.Repeat("0", domain.InstructionsDigestLength), &no},
		{"an agent with none", "ag-3", "0123456789ab", nil},
	} {
		got, err := svc.runsPinnedInstructions(context.Background(), "pe-1", c.agent, c.digest)
		if err != nil || (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}
