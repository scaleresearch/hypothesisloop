package apiclient_test

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestTheKindOfCallerIsReadFromADerivedTokenAndNothingElse(t *testing.T) {
	for token, want := range map[string]apiclient.CallerKind{
		clitest.DerivedToken("agent:pe-1/ag-1"):  apiclient.CallerAgent,
		clitest.DerivedToken("coordinator:pe-1"): apiclient.CallerCoordinator,
		clitest.DerivedToken("staff"):            apiclient.CallerOther,
		"hlk_an-api-key":                         apiclient.CallerOther,
		"hls_not base64.x":                       apiclient.CallerOther,
		"hls_nodot":                              apiclient.CallerOther,
		"":                                       apiclient.CallerOther,
	} {
		if got := apiclient.KindOf(token); got != want {
			t.Errorf("KindOf(%q) = %v, want %v", token, got, want)
		}
	}
}

// KindOf reads the platform's own token format, so it is held to what the platform mints.
func TestTheKindOfCallerIsReadFromTheTokensThePlatformMints(t *testing.T) {
	const secret = "a-signing-secret-only-this-test-knows"
	for scope, want := range map[string]apiclient.CallerKind{
		scope.Agent("pe-1", "ag-1"): apiclient.CallerAgent,
		scope.Coordinator("pe-1"):   apiclient.CallerCoordinator,
		"staff":                     apiclient.CallerOther,
	} {
		token, err := auth.Mint(secret, scope)
		if err != nil {
			t.Fatal(err)
		}
		if got := apiclient.KindOf(token); got != want {
			t.Errorf("KindOf(the token minted for %q) = %v, want %v", scope, got, want)
		}
	}
}
