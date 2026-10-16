package agenttypes_test

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// releases is the digest of what each release seeds (agenttypes.Digest). A change of a built-in or
// of the rules fails this until it is shipped under the next release, with its digest added here.
var releases = map[int]string{
	1: "e23db9a0d4928e952b5aa754c9626f5343202b8f82e2a53c45a0b7c4cdaa3177",
}

func TestReleaseNamesWhatItSeeds(t *testing.T) {
	all, err := agenttypes.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := agenttypes.Digest(all, apidocs.PlatformRules)
	if err != nil {
		t.Fatal(err)
	}
	if want, ok := releases[agenttypes.Release]; !ok || want != digest {
		t.Fatalf("release %d seeds %s, which releases records as %q: a built-in or the rules changed, so raise agenttypes.Release to %d and record %d: %q",
			agenttypes.Release, digest, want, agenttypes.Release+1, agenttypes.Release+1, digest)
	}
	for release := range releases {
		if release > agenttypes.Release {
			t.Errorf("releases records %d, after agenttypes.Release %d", release, agenttypes.Release)
		}
	}
}
