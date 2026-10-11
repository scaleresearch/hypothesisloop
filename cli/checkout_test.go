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

// The first checkout of a hypothesis makes the agent's branch from what the hypothesis builds
// on, and every later one returns to that branch as it is, here or from origin.
func TestHypothesisCheckoutCreatesTheBranchFromTheBaseAndThenReturnsToIt(t *testing.T) {
	clone, origin := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "the parent's commit")
	base := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/hl/pe-1/researcher-2/parent-ffffff")
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "unrelated work")
	seen := checkoutServer(t, origin+"@"+base, "")

	code, stdout, stderr := checkout(checkoutHypothesis)
	if want := `{"branch":"` + checkoutBranchName + `","base":"` + base + `"}`; code != 0 || stdout != want {
		t.Fatalf("first checkout: exit code = %d, stdout = %s, stderr = %s, want %s", code, stdout, stderr, want)
	}
	if on, at := mustGit(t, clone, "symbolic-ref", "--short", "HEAD"), mustGit(t, clone, "rev-parse", "HEAD"); on != checkoutBranchName || at != base {
		t.Fatalf("the clone is on %s at %s, want %s at the base %s", on, at, checkoutBranchName, base)
	}

	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "my change")
	mine := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "checkout", "-q", testBranch)
	code, stdout, stderr = checkout(checkoutHypothesis)
	if want := `{"branch":"` + checkoutBranchName + `"}`; code != 0 || stdout != want || mustGit(t, clone, "rev-parse", "HEAD") != mine {
		t.Fatalf("second checkout: exit code = %d, stdout = %s, stderr = %s, want %s with my change kept", code, stdout, stderr, want)
	}
	if code, _, stderr := checkout(checkoutHypothesis, "--from", "job-other"); code != 1 || !strings.Contains(stderr, "exists") {
		t.Errorf("--from for a branch that exists: exit code = %d, stderr = %s, want it refused", code, stderr)
	}

	// A branch only origin has, as in a fresh clone, is checked out from there.
	mustGit(t, clone, "push", "-q", "origin", checkoutBranchName)
	mustGit(t, clone, "checkout", "-q", testBranch)
	mustGit(t, clone, "branch", "-q", "-D", checkoutBranchName)
	if code, stdout, stderr := checkout(checkoutHypothesis); code != 0 || stdout != `{"branch":"`+checkoutBranchName+`"}` || mustGit(t, clone, "rev-parse", "HEAD") != mine {
		t.Fatalf("checkout of a branch origin has: exit code = %d, stdout = %s, stderr = %s, want my pushed change", code, stdout, stderr)
	}

	// Each checkout that put the clone on the branch was recorded as ag-1 working on it, and
	// one of a hypothesis another agent works on says who, and still checks out.
	if len(seen.recorded) != 3 || seen.recorded[0]["agent_id"] != "ag-1" || seen.recorded[0]["branch"] != checkoutBranchName {
		t.Fatalf("checkouts recorded = %v, want the three that checked out, as ag-1 on %s", seen.recorded, checkoutBranchName)
	}
	seen.others = []map[string]string{{"agent_id": "ag-2", "name": "Researcher 2"}}
	code, stdout, stderr = checkout(checkoutHypothesis)
	if want := `{"branch":"` + checkoutBranchName + `","working":[{"agent_id":"ag-2","name":"Researcher 2"}],"warning":"Researcher 2 already works on hypothesis ` + checkoutHypothesis + `"}`; code != 0 || stdout != want {
		t.Fatalf("checkout of a hypothesis another agent works on: exit code = %d, stdout = %s, stderr = %s, want %s", code, stdout, stderr, want)
	}
}

// --from starts the branch from one particular job's commit, another agent's implementation
// included. With nothing to start from the checkout says so and makes no branch.
func TestHypothesisCheckoutStartsFromAJobOrSaysThereIsNoBase(t *testing.T) {
	clone, origin := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "another agent's implementation")
	other := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "push", "-q", "origin", "HEAD:refs/heads/hl/pe-1/researcher-2/fp16-at-batch-32-a1b2c3")
	mustGit(t, clone, "reset", "-q", "--hard", "HEAD~1")
	checkoutServer(t, "", origin+"@"+other)

	if code, _, stderr := checkout(checkoutHypothesis); code != 1 || !strings.Contains(stderr, "no baseline is declared") || !strings.Contains(stderr, "--from JOB") {
		t.Fatalf("no base: exit code = %d, stderr = %s, want it refused naming the baseline and --from", code, stderr)
	}
	if code, _, stderr := checkout(checkoutHypothesis, "--from", "job-elsewhere"); code != 1 || !strings.Contains(stderr, "of the same one") {
		t.Fatalf("--from a job of another platform experiment: exit code = %d, stderr = %s, want it refused", code, stderr)
	}
	if on := mustGit(t, clone, "symbolic-ref", "--short", "HEAD"); on != testBranch {
		t.Fatalf("a refused checkout left the clone on %s, want %s", on, testBranch)
	}
	code, stdout, stderr := checkout(checkoutHypothesis, "--from", "job-other")
	if want := `{"branch":"` + checkoutBranchName + `","base":"` + other + `"}`; code != 0 || stdout != want || mustGit(t, clone, "rev-parse", "HEAD") != other {
		t.Fatalf("--from: exit code = %d, stdout = %s, stderr = %s, want %s", code, stdout, stderr, want)
	}
}

// Researchers wrote files in their clone before they checked out a hypothesis, and git's
// own refusal sent them into a stash dance that failed too. The refusal says what to do.
func TestHypothesisCheckoutOverChangesSaysToCommitOrStashThem(t *testing.T) {
	clone, origin := pushedClone(t)
	if err := os.WriteFile(filepath.Join(clone, "metrics.py"), []byte("baseline's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, clone, "add", "metrics.py")
	mustGit(t, clone, "commit", "-q", "-m", "the baseline")
	base := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "reset", "-q", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(clone, "metrics.py"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkoutServer(t, origin+"@"+base, "")

	code, _, stderr := checkout(checkoutHypothesis)
	if code != 1 || !strings.Contains(stderr, "Commit them (git add -A && git commit) or stash them (git stash -u), then run hl hypothesis checkout again") || strings.Contains(stderr, "exit status") {
		t.Fatalf("checkout over an untracked file: exit code = %d, stderr = %s, want it refused saying to commit or stash", code, stderr)
	}
	if on := mustGit(t, clone, "symbolic-ref", "--short", "HEAD"); on != testBranch || read(t, clone, "metrics.py") != "mine\n" {
		t.Fatalf("a refused checkout left the clone on %s, want %s with the file untouched", on, testBranch)
	}
}
