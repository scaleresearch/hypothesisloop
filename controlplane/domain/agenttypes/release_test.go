package agenttypes_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain/agenttypes"
)

// releases is the digest of what each release seeds (releaseDigest). A change of a built-in or
// of the rules fails this until it is shipped under the next release, with its digest added here.
var releases = map[int]string{
	1: "e23db9a0d4928e952b5aa754c9626f5343202b8f82e2a53c45a0b7c4cdaa3177",
	2: "f72f2a37fd20ab4461fa45e04101e93060db4025b5ea06bac6382ad18347c73d",
	3: "dad6a1db341ee65a6cc83a8d36ef4f28e4d37fe475b103a805f6572d6c79df37",
	4: "e5955152cad00587a228bb531b21ab1e1b7df11988271708d7c05f2cf8918cdc",
	5: "93d05ee21009b1136d0fe1adc031770ac8425461bbf2d84d3b3f44e1ea2534a4",
	6: "e5c1b2971bf5f721acf949af66a22d8abaaf1102b984bf625cf5ce9d87ec1467",
	7: "39eee57d7ee86129ac029c0496eba4f04191a5aae0338a6d959d2564c8a8e349",
}

func TestReleaseNamesWhatItSeeds(t *testing.T) {
	all, err := agenttypes.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := releaseDigest(all, agenttypes.PlatformRules)
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

// releaseDigest is sha256 of everything a release seeds: each built-in's id, name, description, role,
// prompt and settings, and rules.
func releaseDigest(all []domain.BuiltinAgentType, rules string) (string, error) {
	sum := sha256.New()
	for _, b := range all {
		settings, err := json.Marshal(b.Settings)
		if err != nil {
			return "", err
		}
		for _, part := range []string{b.ID, b.Name, b.Description, string(b.Role), b.Prompt, string(settings)} {
			sum.Write([]byte(part))
			sum.Write([]byte{0})
		}
	}
	sum.Write([]byte(rules))
	return hex.EncodeToString(sum.Sum(nil)), nil
}
