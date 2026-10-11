package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

const checkoutHypothesis = "0198c2d4-aaaa-7bbb-8ccc-000000a1b2c3"
const checkoutBranchName = "hl/pe-1/researcher-1/fp16-at-batch-32-a1b2c3"

// checkouts is what the platform was told of checkouts, and the agents it answers work on the
// hypothesis besides.
type checkouts struct {
	recorded []map[string]string
	others   []map[string]string
}

// checkoutServer answers the reads hl hypothesis checkout makes as agent ag-1 of pe-1 behind the
// platform's own check of what an agent's token may call: the hypothesis with baseRef, the
// agent's signup naming it Researcher 1, and job-other with otherRef. It records each checkout.
func checkoutServer(t *testing.T, baseRef, otherRef string) *checkouts {
	t.Helper()
	token := derivedToken("agent:pe-1/ag-1")
	seen := &checkouts{}
	setAPIURL(t, withServer(t, asThePlatformAdmits(t, token, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hypotheses/" + checkoutHypothesis + "/checkouts":
			var body map[string]string
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Errorf("checkout record: %s with an unreadable body", r.Method)
			}
			seen.recorded = append(seen.recorded, body)
			answer := map[string]any{"hypothesis_id": checkoutHypothesis, "agent_id": "ag-1", "branch": body["branch"], "working": append([]map[string]string{}, seen.others...)}
			if len(seen.others) > 0 {
				answer["warning"] = "Researcher 2 already works on hypothesis " + checkoutHypothesis
			}
			_ = json.NewEncoder(w).Encode(answer)
		case "/hypotheses/" + checkoutHypothesis:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": checkoutHypothesis, "platform_experiment_id": "pe-1", "text": "FP16 at batch 32", "base_ref": baseRef})
		case "/platform-experiments/pe-1/signups/ag-1":
			_ = json.NewEncoder(w).Encode(map[string]string{"agent_id": "ag-1", "name": "Researcher 1"})
		case "/experiments/job-other":
			_ = json.NewEncoder(w).Encode(map[string]string{"platform_experiment_id": "pe-1", "code_ref": otherRef})
		case "/experiments/job-elsewhere":
			_ = json.NewEncoder(w).Encode(map[string]string{"platform_experiment_id": "pe-2", "code_ref": otherRef})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})))
	t.Setenv("HL_TOKEN", token)
	return seen
}

func checkout(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"hypothesis", "checkout"}, args...), &stdout, &stderr)
	return code, strings.TrimSpace(stdout.String()), stderr.String()
}

// checkoutHome makes a home of its own for the test and answers the clone hl hypothesis checkout
// makes there for checkoutBranchName.
func checkoutHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, "hl-checkouts", filepath.FromSlash(checkoutBranchName))
}

// The first checkout of a hypothesis makes the agent's branch from what the hypothesis builds
// on, in a clone of its own, and every later one returns to that clone as it is, or makes it
// again from origin. The clone it runs in stays as it was.
func TestHypothesisCheckoutCreatesTheBranchInAClone(t *testing.T) {
	clone, origin := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "the parent's commit")
	base := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/hl/pe-1/researcher-2/parent-ffffff")
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "unrelated work")
	before := mustGit(t, clone, "rev-parse", "HEAD")
	seen := checkoutServer(t, origin+"@"+base, "")
	dir := checkoutHome(t)
	answer := func(extra string) string {
		return `{"branch":"` + checkoutBranchName + `","dir":"` + dir + `"` + extra + `}`
	}

	code, stdout, stderr := checkout(checkoutHypothesis)
	if want := answer(`,"base":"` + base + `"`); code != 0 || stdout != want {
		t.Fatalf("first checkout: exit code = %d, stdout = %s, stderr = %s, want %s", code, stdout, stderr, want)
	}
	if on, at := mustGit(t, dir, "symbolic-ref", "--short", "HEAD"), mustGit(t, dir, "rev-parse", "HEAD"); on != checkoutBranchName || at != base {
		t.Fatalf("the branch's clone is on %s at %s, want %s at the base %s", on, at, checkoutBranchName, base)
	}
	if on, at := mustGit(t, clone, "symbolic-ref", "--short", "HEAD"), mustGit(t, clone, "rev-parse", "HEAD"); on != testBranch || at != before {
		t.Fatalf("the clone it ran in is on %s at %s, want it left on %s at %s", on, at, testBranch, before)
	}
	if url := mustGit(t, dir, "remote", "get-url", "origin"); url != origin {
		t.Fatalf("the branch's clone pushes to %s, want %s", url, origin)
	}

	// Work in progress in the branch's clone stays through another checkout, as after a restart.
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "my change")
	mine := mustGit(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "draft.py"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = checkout(checkoutHypothesis)
	if want := answer(""); code != 0 || stdout != want || mustGit(t, dir, "rev-parse", "HEAD") != mine || read(t, dir, "draft.py") != "mine\n" {
		t.Fatalf("second checkout: exit code = %d, stdout = %s, stderr = %s, want %s with my work kept", code, stdout, stderr, want)
	}
	if code, _, stderr := checkout(checkoutHypothesis, "--from", "job-other"); code != 1 || !strings.Contains(stderr, "exists") {
		t.Errorf("--from for a branch that exists: exit code = %d, stderr = %s, want it refused", code, stderr)
	}

	// A branch only origin has, as on a new machine, is cloned again from there.
	mustGit(t, dir, "push", "-q", "origin", checkoutBranchName)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := checkout(checkoutHypothesis); code != 0 || stdout != answer("") || mustGit(t, dir, "rev-parse", "HEAD") != mine {
		t.Fatalf("checkout of a branch origin has: exit code = %d, stdout = %s, stderr = %s, want my pushed change", code, stdout, stderr)
	}

	// Each checkout that put the branch in its clone was recorded as ag-1 working on it, and
	// one of a hypothesis another agent works on says who, and still checks out.
	if len(seen.recorded) != 3 || seen.recorded[0]["agent_id"] != "ag-1" || seen.recorded[0]["branch"] != checkoutBranchName {
		t.Fatalf("checkouts recorded = %v, want the three that checked out, as ag-1 on %s", seen.recorded, checkoutBranchName)
	}
	seen.others = []map[string]string{{"agent_id": "ag-2", "name": "Researcher 2"}}
	code, stdout, stderr = checkout(checkoutHypothesis)
	if want := answer(`,"working":[{"agent_id":"ag-2","name":"Researcher 2"}],"warning":"Researcher 2 already works on hypothesis ` + checkoutHypothesis + `"`); code != 0 || stdout != want {
		t.Fatalf("checkout of a hypothesis another agent works on: exit code = %d, stdout = %s, stderr = %s, want %s", code, stdout, stderr, want)
	}
}

// --from starts the branch from one particular job's commit, another agent's implementation
// included. With nothing to start from the checkout says so and leaves no clone.
func TestHypothesisCheckoutStartsFromAJobOrSaysThereIsNoBase(t *testing.T) {
	clone, origin := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "another agent's implementation")
	other := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/hl/pe-1/researcher-2/fp16-at-batch-32-a1b2c3")
	mustGit(t, clone, "reset", "-q", "--hard", "HEAD~1")
	checkoutServer(t, "", origin+"@"+other)
	dir := checkoutHome(t)

	if code, _, stderr := checkout(checkoutHypothesis); code != 1 || !strings.Contains(stderr, "no baseline is declared") || !strings.Contains(stderr, "--from JOB") {
		t.Fatalf("no base: exit code = %d, stderr = %s, want it refused naming the baseline and --from", code, stderr)
	}
	if code, _, stderr := checkout(checkoutHypothesis, "--from", "job-elsewhere"); code != 1 || !strings.Contains(stderr, "of the same one") {
		t.Fatalf("--from a job of another platform experiment: exit code = %d, stderr = %s, want it refused", code, stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refused checkout left %s: %v", dir, err)
	}
	code, stdout, stderr := checkout(checkoutHypothesis, "--from", "job-other")
	if want := `{"branch":"` + checkoutBranchName + `","dir":"` + dir + `","base":"` + other + `"}`; code != 0 || stdout != want || mustGit(t, dir, "rev-parse", "HEAD") != other {
		t.Fatalf("--from: exit code = %d, stdout = %s, stderr = %s, want %s", code, stdout, stderr, want)
	}
}
