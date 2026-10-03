package boundedname

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

const (
	dnsLabelLength = 63
	// Two ids of one millisecond: a UUIDv7's first 12 hex digits are its timestamp.
	jobID        = "job-0199c2a4b7d07f3a9c1e5b2d4f608a13"
	jobIDSameMs  = "job-0199c2a4b7d07e11a04c7d9e3b5f2c68"
	experimentID = "pe-0199c2a4b7d07f3a9c1e5b2d4f608a13"
	agentID      = "ag-0199c2a4b7d07f3a9c1e5b2d4f608a13"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestANameWithinItsLimitIsKeptAsItIs(t *testing.T) {
	for _, id := range []string{jobID, experimentID, agentID} {
		if got := Name("exp-"+id, id, dnsLabelLength); got != "exp-"+id {
			t.Errorf("Name(%q) = %q, want it unchanged: it is %d characters", id, got, len("exp-"+id))
		}
	}
	exact := strings.Repeat("a", dnsLabelLength)
	if got := Name(exact, exact, dnsLabelLength); got != exact {
		t.Errorf("a name exactly at the limit was changed to %q", got)
	}
}

func TestACutNameFitsItsLimitAndStaysADNSLabel(t *testing.T) {
	longestID := strings.Repeat("a", 62) + "b"
	for _, limit := range []int{dnsLabelLength, 61, 57, 52} {
		for _, readable := range []string{
			"exp-" + longestID,
			"exp-" + jobID + "-" + strings.Repeat("parameter-server-", 4) + "worker",
			"exp-" + jobID + "-" + strings.Repeat("w", 200),
		} {
			got := Name(readable, readable, limit)
			if len(got) != limit || !dnsLabel.MatchString(got) {
				t.Errorf("limit %d: Name(%q) = %q, %d characters", limit, readable, got, len(got))
			}
			if again := Name(readable, readable, limit); again != got {
				t.Errorf("limit %d: Name(%q) gave %q and then %q", limit, readable, got, again)
			}
		}
	}
}

func TestCutNamesOfIdsMadeInOneMillisecondDiffer(t *testing.T) {
	group := "-" + strings.Repeat("parameter-server-", 3) + "worker"
	// The cut falls inside the id for the first limit and after it for the second.
	for _, limit := range []int{30, dnsLabelLength} {
		one := Name("exp-"+jobID+group, jobID+"/"+group, limit)
		other := Name("exp-"+jobIDSameMs+group, jobIDSameMs+"/"+group, limit)
		if len(one) != limit || len(other) != limit || one == other {
			t.Errorf("limit %d: %q and %q name two jobs", limit, one, other)
		}
	}
}

func TestCutNamesOfGroupsThatShareTheirStartDiffer(t *testing.T) {
	shared := strings.Repeat("parameter-server-", 3)
	seen := map[string]string{}
	for _, group := range []string{shared + "worker", shared + "chief", shared + "evaluator"} {
		got := Name("exp-"+jobID+"-"+group, jobID+"/"+group, dnsLabelLength)
		if earlier, taken := seen[got]; taken {
			t.Errorf("groups %q and %q are both named %q", earlier, group, got)
		}
		seen[got] = group
	}
}

// A pod of an Indexed Job is reached at <job name>-<rank>, which is one DNS label.
func TestEveryRankOfAJobWithTheLongestIdsHasAHostnameThatIsADNSLabel(t *testing.T) {
	longestID := strings.Repeat("a", 62) + "b"
	for _, id := range []string{jobID, longestID} {
		for _, group := range []string{"", "actor", strings.Repeat("parameter-server-", 3) + "worker"} {
			for _, ranks := range []int{1, 2, 10, 1000, 100000} {
				last := fmt.Sprintf("-%d", ranks-1)
				readable := "exp-" + id
				if group != "" {
					readable += "-" + group
				}
				name := Name(readable, id+"/"+group, dnsLabelLength-len(last))
				if hostname := name + last; len(hostname) > dnsLabelLength || !dnsLabel.MatchString(hostname) {
					t.Errorf("id %q group %q with %d ranks: hostname %q is %d characters", id, group, ranks, hostname, len(hostname))
				}
			}
		}
	}
}
