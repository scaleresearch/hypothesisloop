package db

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// schemaList is the SQL list the statement of schema.sql that starts with head holds after
// "<column> IN ", as written.
func schemaList(t *testing.T, head, column string) string {
	t.Helper()
	at := strings.Index(schemaSQL, head)
	if at < 0 {
		t.Fatalf("schema.sql has no %q", head)
	}
	rest := schemaSQL[at:]
	rest = rest[:strings.Index(rest, ";")]
	in := strings.Index(rest, column+" IN ")
	if in < 0 {
		t.Fatalf("%s: no %s IN list", head, column)
	}
	list := rest[in+len(column)+len(" IN "):]
	return list[:strings.Index(list, ")")+1]
}

// quoted is the values of an SQL list, in their order.
func quoted(list string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(list, -1) {
		out = append(out, m[1])
	}
	return out
}

func asStrings[S ~string](values []S) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// endedSQL and placedSQL are the predicates of the partial indexes on experiments, written as the
// index writes them: a query that spells them otherwise does not use the index.
func TestTheStatusListsAreThePredicatesOfThePartialIndexes(t *testing.T) {
	if got := schemaList(t, "CREATE INDEX idx_experiments_desired ", "status"); got != placedSQL {
		t.Errorf("idx_experiments_desired holds %s, placedSQL is %s", got, placedSQL)
	}
	if got := schemaList(t, "CREATE INDEX idx_experiments_unsettled ", "status"); got != endedSQL {
		t.Errorf("idx_experiments_unsettled holds %s, endedSQL is %s", got, endedSQL)
	}
	if got := schemaList(t, "CREATE INDEX idx_experiments_data_key ", "status"); got != endedSQL {
		t.Errorf("idx_experiments_data_key holds %s, endedSQL is %s", got, endedSQL)
	}
	if got := schemaList(t, "CREATE INDEX idx_platform_experiments_in_progress ", "status"); !slices.Equal(quoted(got), asStrings(slices.DeleteFunc(slices.Clone(domain.PlatformExperimentStatuses), func(s domain.PlatformExperimentStatus) bool {
		return s == domain.PlatformExpClosed
	}))) {
		t.Errorf("idx_platform_experiments_in_progress holds %s, want every status but closed", got)
	}
}

// Every stored status is in exactly one of the lists of not ended and ended, and the placed and
// unstarted ones are among those not ended. No row holds cancelled.
func TestEveryStoredStatusIsUnfinishedOrEndedAndNeverBoth(t *testing.T) {
	unfinished, ended := quoted(unfinishedSQL), quoted(endedSQL)
	var stored []string
	for _, s := range domain.ExperimentStatuses {
		if s == domain.StatusCancelled {
			if slices.Contains(unfinished, string(s)) || slices.Contains(ended, string(s)) {
				t.Errorf("%s is in a stored list", s)
			}
			continue
		}
		stored = append(stored, string(s))
		if slices.Contains(unfinished, string(s)) == slices.Contains(ended, string(s)) {
			t.Errorf("%s: unfinished %v, ended %v, want exactly one", s, slices.Contains(unfinished, string(s)), slices.Contains(ended, string(s)))
		}
		if slices.Contains(ended, string(s)) != s.Ended() {
			t.Errorf("%s: in endedSQL %v, Ended() %v", s, slices.Contains(ended, string(s)), s.Ended())
		}
	}
	for name, sub := range map[string]string{"placedSQL": placedSQL, "unstartedSQL": unstartedSQL} {
		for _, s := range quoted(sub) {
			if !slices.Contains(unfinished, s) {
				t.Errorf("%s holds %s, which has ended", name, s)
			}
		}
	}
	if got := quoted(schemaList(t, "CONSTRAINT experiments_status", "status")); !slices.Equal(got, stored) {
		t.Errorf("experiments_status allows %v, the stored statuses are %v", got, stored)
	}
	if got := quoted(schemaList(t, "CONSTRAINT experiments_active_has_data_key", "status")); !slices.Equal(got, ended) {
		t.Errorf("experiments_active_has_data_key exempts %v, the ended statuses are %v", got, ended)
	}
	if !strings.Contains(reservedSQL, unfinishedSQL) || !strings.Contains(reservedSQL, endedSQL) {
		t.Errorf("reservedSQL %s reads neither list as written", reservedSQL)
	}
}

// What schema.sql allows in a column with a fixed set of values is the Go list of them, in order.
func TestTheSchemasValueChecksAreTheGoLists(t *testing.T) {
	actorKinds := asStrings(domain.ActorKinds)
	instanceEnds := []string{"", string(InstanceExited), string(InstanceGone), string(InstanceNeverSeen), string(InstanceStillStarting),
		string(InstanceSilent), string(InstanceProgressUnreadable), string(InstanceExpired), string(InstanceReleased)}
	for _, c := range []struct {
		head, column string
		want         []string
	}{
		{"CONSTRAINT platform_experiments_status", "status", asStrings(domain.PlatformExperimentStatuses)},
		{"CONSTRAINT org_members_role", "role", asStrings(domain.OrgRoles)},
		{"CONSTRAINT org_invites_role", "role", asStrings(domain.OrgRoles)},
		{"CONSTRAINT agent_types_role", "role", asStrings(domain.AgentRoles)},
		{"CONSTRAINT experiment_signups_role", "role", asStrings(domain.SignupRoles)},
		{"CONSTRAINT experiments_capacity_tier", "capacity_tier", []string{string(domain.CapacityGuaranteed), string(domain.CapacityBurst)}},
		{"CONSTRAINT experiment_signups_quota_tier", "quota_tier", []string{string(domain.QuotaTierGuaranteed), string(domain.QuotaTierBurstOnly)}},
		{"CONSTRAINT platform_experiments_default_quota_tier", "default_quota_tier", []string{string(domain.QuotaTierGuaranteed), string(domain.QuotaTierBurstOnly)}},
		{"CONSTRAINT experiments_actor_kind", "actor_kind", actorKinds},
		{"CONSTRAINT platform_experiments_removed_by_kind", "removed_by_kind", append([]string{""}, actorKinds...)},
		{"CONSTRAINT agentsandbox_instances_state", "state", []string{"", domain.InstanceStarting, domain.InstanceRunning}},
		{"CONSTRAINT agentsandbox_instances_end_reason", "end_reason", instanceEnds},
	} {
		if got := quoted(schemaList(t, c.head, c.column)); !slices.Equal(got, c.want) {
			t.Errorf("%s allows %v, the Go list is %v", c.head, got, c.want)
		}
	}
}
