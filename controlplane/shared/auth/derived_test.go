package auth

import (
	"regexp"
	"strings"
	"testing"
)

const testSecret = "test-signing-secret-of-enough-length"

func TestMintedTokensVerifyToTheScopeTheyWereMintedFor(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  Principal
	}{
		{StaffScope, Principal{Kind: KindStaff}},
		{JobScope("job-1"), Principal{Kind: KindJob, ExperimentID: "job-1"}},
		{AgentScope("pe-1", "agent-1"), Principal{Kind: KindAgent, PlatformExperimentID: "pe-1", AgentID: "agent-1"}},
		{CoordinatorScope("pe-1"), Principal{Kind: KindCoordinator, PlatformExperimentID: "pe-1"}},
		{RuntimeScope("local"), Principal{Kind: KindRuntime, ClusterName: "local"}},
		{BuildScope("local", "abc123", 2), Principal{Kind: KindBuild, Build: BuildRef{Cluster: "local", Key: "abc123", Attempt: 2}}},
	} {
		token, err := Mint(testSecret, tc.scope)
		if err != nil {
			t.Fatalf("mint %s: got = %v, want = nil", tc.scope, err)
		}
		again, _ := Mint(testSecret, tc.scope)
		if again != token {
			t.Errorf("mint %s twice: got two different tokens, want the same one", tc.scope)
		}
		got, err := verifyDerived(testSecret, token)
		if err != nil || !samePrincipal(got, tc.want) {
			t.Errorf("verify %s: got = %+v, %v, want = %+v", tc.scope, got, err, tc.want)
		}
	}
}

// The token rides in Sec-WebSocket-Protocol, whose value must be an HTTP token: no ':' or '/'.
func TestMintedTokenIsOneHTTPToken(t *testing.T) {
	token, err := Mint(testSecret, AgentScope("pe-1", "agent.with/odd:chars"))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$").MatchString(token) {
		t.Fatalf("token %q is not a single HTTP token", token)
	}
}

func TestForgedAndMalformedDerivedTokensAreRefused(t *testing.T) {
	staff, _ := Mint(testSecret, StaffScope)
	job, _ := Mint(testSecret, JobScope("job-1"))
	scopePart, macPart, _ := strings.Cut(strings.TrimPrefix(job, derivedPrefix), ".")
	staffScopePart, _, _ := strings.Cut(strings.TrimPrefix(staff, derivedPrefix), ".")

	for name, token := range map[string]string{
		"signed with another secret":     mustMint(t, "another-signing-secret-of-enough-length", StaffScope),
		"job mac under the staff scope":  derivedPrefix + staffScopePart + "." + macPart,
		"scope edited after signing":     derivedPrefix + encoding.EncodeToString([]byte("job:job-2")) + "." + macPart,
		"mac truncated":                  derivedPrefix + scopePart + "." + macPart[:len(macPart)-2],
		"no mac":                         derivedPrefix + scopePart,
		"empty mac":                      derivedPrefix + scopePart + ".",
		"scope not base64":               derivedPrefix + "job:job-1." + macPart,
		"mac not base64":                 derivedPrefix + scopePart + ".!!!",
		"prefix only":                    derivedPrefix,
		"correctly signed unknown scope": derivedPrefix + encoding.EncodeToString([]byte("root")) + "." + encoding.EncodeToString(mac(testSecret, "root")),
		"correctly signed empty subject": derivedPrefix + encoding.EncodeToString([]byte("job:")) + "." + encoding.EncodeToString(mac(testSecret, "job:")),
	} {
		if p, err := verifyDerived(testSecret, token); err == nil {
			t.Errorf("%s: got principal %+v, want a refusal", name, p)
		}
	}
}

func TestMintRefusesWhatItCouldNotVerify(t *testing.T) {
	for _, scope := range []string{"", "root", "job:", "agent:pe-1", "agent:/agent-1", "agent:pe-1/", "unknown:x", "build:local/abc", "build:local/abc/0", "build:local/abc/x", "build:local/abc/01", "build:/abc/1", "build:local//1", "build:a/b/c/1"} {
		if token, err := Mint(testSecret, scope); err == nil {
			t.Errorf("mint %q: got token %q, want an error", scope, token)
		}
	}
	if _, err := Mint("", StaffScope); err == nil {
		t.Error("mint with an empty secret: got = nil, want an error")
	}
}

func mustMint(t *testing.T, secret, scope string) string {
	t.Helper()
	token, err := Mint(secret, scope)
	if err != nil {
		t.Fatalf("mint %s: %v", scope, err)
	}
	return token
}

func samePrincipal(a, b Principal) bool {
	return a.Kind == b.Kind && a.UserID == b.UserID && a.Name == b.Name && a.ProjectID == b.ProjectID &&
		a.PlatformExperimentID == b.PlatformExperimentID && a.AgentID == b.AgentID && a.ExperimentID == b.ExperimentID &&
		a.ClusterName == b.ClusterName && a.Build == b.Build && strings.Join(a.OrgIDs, ",") == strings.Join(b.OrgIDs, ",")
}

func TestAPIKeysAreDistinctAndHashStably(t *testing.T) {
	id1, token1, err := NewAPIKey()
	if err != nil {
		t.Fatalf("new api key: %v", err)
	}
	id2, token2, _ := NewAPIKey()
	if id1 == id2 || token1 == token2 {
		t.Fatal("two generated keys are equal")
	}
	if !strings.HasPrefix(token1, apiKeyPrefix+id1+"_") {
		t.Fatalf("token %q does not carry its id %q", token1, id1)
	}
	if HashAPIKey(token1) != HashAPIKey(token1) || HashAPIKey(token1) == HashAPIKey(token2) {
		t.Fatal("hash is not a stable function of the token")
	}
	if strings.Contains(HashAPIKey(token1), token1[len(apiKeyPrefix)+len(id1)+1:]) {
		t.Fatal("hash contains the secret")
	}
}
