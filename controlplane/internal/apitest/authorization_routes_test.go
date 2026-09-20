package apitest

import (
	"net/http"
	"strings"
)

// rule is the answer a route gives a caller for an object of the given owner.
type rule func(principal, owner string) int

// The audiences, as the callers of this table. Staff, who pass every one, are not in it.
var (
	agentAudience       = []string{"user", "org key", "project key", "agent", "coordinator"}
	jobAudience         = []string{"user", "org key", "project key", "agent", "coordinator", "job"}
	coordinatorAudience = []string{"user", "org key", "project key", "coordinator"}
	people              = []string{"user", "org key", "project key"}
	orgWide             = []string{"user", "org key"}
)

func among(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// holds says whether the caller's scope holds an object of the owner: a member or a key of
// org-a everything in org-a, and a token bound to prj-a1 or to pe-a1 only what is in pe-a1.
func holds(principal, owner string) bool {
	switch owner {
	case "own", "peer":
		return true
	case "sibling":
		return among(orgWide, principal)
	}
	return false
}

// gate is the two answers every route gives before its handler runs: no token, and a token of
// a kind the route's audience does not admit.
func gate(audience []string, principal string) (int, bool) {
	if principal == "none" {
		return http.StatusUnauthorized, true
	}
	if !among(audience, principal) {
		return http.StatusForbidden, true
	}
	return 0, false
}

// listed is a route that names no object, or narrows a listing by one: the audience admits the
// caller and the listing itself is drawn from the caller's scope, which the store tests hold.
func listed(audience []string, admitted int) rule {
	return func(principal, _ string) int {
		if status, refused := gate(audience, principal); refused {
			return status
		}
		return admitted
	}
}

// scoped is a route on an object placed in a platform experiment: outside the caller's scope
// it is not found, whether or not it exists.
func scoped(audience []string, admitted int) rule {
	return func(principal, owner string) int {
		if status, refused := gate(audience, principal); refused {
			return status
		}
		if !holds(principal, owner) {
			return http.StatusNotFound
		}
		return admitted
	}
}

// owned is scoped, and an agent token acts only on what is its own agent's.
func owned(audience []string, admitted int) rule {
	inScope := scoped(audience, admitted)
	return func(principal, owner string) int {
		status := inScope(principal, owner)
		if status == admitted && principal == "agent" && owner != "own" {
			return http.StatusForbidden
		}
		return status
	}
}

// spendsAs is a rule for a request that spends as the agent it names: a coordinator's token is
// refused where it would otherwise be admitted, since it acts as no agent.
func spendsAs(admits rule) rule {
	return func(principal, owner string) int {
		status := admits(principal, owner)
		if principal == "coordinator" && status == reached {
			return http.StatusForbidden
		}
		return status
	}
}

// ownedOrTheJob is owned, and the job's own token reaches that one job.
func ownedOrTheJob(admitted int) rule {
	others := owned(jobAudience, admitted)
	return func(principal, owner string) int {
		if principal == "job" && owner != "own" {
			return http.StatusForbidden
		}
		return others(principal, owner)
	}
}

// orgLevel is a route on something an org holds (a cluster, the org itself): a token bound to a
// project or to a platform experiment reaches none, which bound says how it is answered.
func orgLevel(audience []string, bound, admitted int) rule {
	return func(principal, owner string) int {
		if status, refused := gate(audience, principal); refused {
			return status
		}
		if owner == "foreign" || owner == "absent" {
			return http.StatusNotFound
		}
		if !among(orgWide, principal) {
			return bound
		}
		return admitted
	}
}

// inOrg is a read of an org's own listing: its members and every key of it.
func inOrg(admitted int) rule {
	return func(principal, owner string) int {
		if status, refused := gate(people, principal); refused {
			return status
		}
		if owner == "foreign" || owner == "absent" {
			return http.StatusNotFound
		}
		return admitted
	}
}

// ofProject is a change to one project: one outside the caller's sight is not found, and of
// the ones it sees an org-wide caller changes it.
func ofProject(admitted int) rule {
	return func(principal, owner string) int {
		if status, refused := gate(people, principal); refused {
			return status
		}
		if owner == "foreign" || owner == "absent" || (principal == "project key" && owner == "sibling") {
			return http.StatusNotFound
		}
		if !among(orgWide, principal) {
			return http.StatusForbidden
		}
		return admitted
	}
}

// only admits the listed callers whatever the object: the handler has to read a row to learn
// whose it is, and its own tests hold that decision.
func only(callers []string, admitted int) rule { return listed(callers, admitted) }

// ofCluster is a runtime's own cluster.
func ofCluster(admitted int) rule {
	return func(principal, owner string) int {
		if principal == "none" {
			return http.StatusUnauthorized
		}
		if principal != "runtime" || owner == "foreign" || owner == "absent" {
			return http.StatusForbidden
		}
		return admitted
	}
}

var staffOnly = only(nil, 0)

// metricsUnreachable is what an admitted caller gets from the three routes that ask the metrics
// database before any store: there is none behind this router.
const metricsUnreachable = http.StatusInternalServerError

const (
	job1      = `{"id":"job-new","metadata":{"platform_experiment_id":"<pe>","agent_id":"<agent>","hypothesis_id":"hyp-own","code_ref":"r@0","config_hash":"h","data_ref":"d","objective":"o","theory":"t","estimated_duration_hours":1},"job":{"image":"i","cpu":"1","memory":"1Gi","storage":"1Gi","max_retries":0}}`
	pe1       = `{"project_id":"<prj>","name":"n","description":"d","budget_accelerator_hours":1,"max_agents":2,"metrics":[{"key":"loss","direction":"minimize","role":"ranking"}],"report_interval_seconds":60,"starts_at":"2030-01-01T00:00:00Z","ends_at":"2030-01-02T00:00:00Z"}`
	capacity1 = `{"cpu_available_cores":1,"cpu_total_cores":1,"accelerator_available_by_type":{},"accelerator_total_by_type":{},"accelerator_available_by_node":{},"node_resources_by_node":{},"node_resources_total_by_node":{},"node_labels_by_node":{},"multi_node_capable":false,"autoscaler_enabled":false,"cluster_id":"","ram_available_bytes":1,"ram_total_bytes":1,"storage_available_bytes":1,"storage_total_bytes":1}`
)

var routes = []route{
	// ---- jobs ----
	{method: "POST", path: "/experiments", body: job1, rule: spendsAs(owned(agentAudience, reached))},
	{method: "GET", path: "/experiments", request: "/experiments?platform_experiment_id=<pe>", rule: listed(agentAudience, reached)},
	{method: "GET", path: "/experiments/stats", request: "/experiments/stats?platform_experiment_id=<pe>", rule: listed(agentAudience, reached)},
	{method: "GET", path: "/experiments/{id}", request: "/experiments/<job>", rule: scoped(agentAudience, reached)},
	{method: "POST", path: "/experiments/{id}/admit", request: "/experiments/<job>/admit", body: `{"cluster_name":"<cluster>"}`, rule: orgLevel(coordinatorAudience, http.StatusNotFound, reached)},
	{method: "POST", path: "/experiments/{id}/cancel", request: "/experiments/<job>/cancel", rule: owned(agentAudience, reached)},
	{method: "POST", path: "/experiments/{id}/summary", request: "/experiments/<job>/summary", body: `{"summary":"s"}`, rule: owned(agentAudience, reached)},
	{method: "POST", path: "/experiments/reprioritize", rule: staffOnly},
	{method: "GET", path: "/experiments/{id}/lineage", request: "/experiments/<job>/lineage", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/experiments/{id}/data", request: "/experiments/<job>/data", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/experiments/{id}/metrics", request: "/experiments/<job>/metrics", rule: scoped(agentAudience, reached)},
	{method: "POST", path: "/experiments/{id}/metrics", request: "/experiments/<job>/metrics", body: `{"metric_name":"loss","fraction_complete":0.5,"metric_value":1}`, rule: ownedOrTheJob(reached)},
	{method: "GET", path: "/experiments/{id}/logs", request: "/experiments/<job>/logs", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/experiments/{id}/cost", request: "/experiments/<job>/cost", rule: scoped(agentAudience, reached)},

	// ---- cluster settings ----
	{method: "GET", path: "/clusters/{cluster_id}/settings", request: "/clusters/<cluster>/settings", rule: orgLevel(coordinatorAudience, http.StatusNotFound, reached)},
	{method: "PUT", path: "/clusters/{cluster_id}/settings", request: "/clusters/<cluster>/settings", body: `{}`, rule: orgLevel(coordinatorAudience, http.StatusNotFound, reached)},

	// ---- hypotheses ----
	{method: "POST", path: "/hypotheses", body: `{"agent_id":"<agent>","platform_experiment_id":"<pe>","text":"t","links":[{"type":"builds_on","to":"<pe>"}]}`, rule: owned(agentAudience, reached)},
	{method: "GET", path: "/hypotheses", request: "/hypotheses?platform_experiment_id=<pe>", rule: listed(agentAudience, reached)},
	{method: "POST", path: "/hypotheses/query", body: `{"platform_experiment_id":"<pe>","cypher":"RETURN 1"}`, rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/hypotheses/graph", request: "/hypotheses/graph?platform_experiment_id=<pe>", rule: scoped(coordinatorAudience, reached)},
	{method: "GET", path: "/hypotheses/stats", request: "/hypotheses/stats?platform_experiment_id=<pe>", rule: listed(coordinatorAudience, reached)},
	{method: "GET", path: "/hypotheses/{id}", request: "/hypotheses/<hyp>", rule: scoped(agentAudience, reached)},
	{method: "POST", path: "/hypotheses/{id}/links", request: "/hypotheses/<hyp>/links", body: `{"agent_id":"<agent>","type":"related","to":"hyp-own"}`, rule: owned(agentAudience, reached)},
	{method: "POST", path: "/hypotheses/{id}/comments", request: "/hypotheses/<hyp>/comments", body: `{"agent_id":"<agent>","text":"t"}`, rule: owned(agentAudience, reached)},
	{method: "POST", path: "/hypotheses/{id}/status", request: "/hypotheses/<hyp>/status", body: `{"agent_id":"<agent>","status":"confirmed"}`, rule: owned(agentAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/findings", request: "/platform-experiments/<pe>/findings", rule: scoped(coordinatorAudience, reached)},

	// ---- watch ----
	{method: "GET", path: "/watch", request: "/watch?platform_experiment_id=<pe>", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/watch/kinds", rule: listed(agentAudience, http.StatusOK)},
	{method: "GET", path: "/watch/subscriptions", request: "/watch/subscriptions?platform_experiment_id=<pe>", rule: listed(agentAudience, http.StatusOK)},

	// ---- agents ----
	{method: "POST", path: "/agents", body: `{"id":"agent-new","org_id":"<org>"}`, rule: inOrgOf(coordinatorAudience, reached)},
	{method: "GET", path: "/agents", rule: listed(coordinatorAudience, reached)},
	{method: "GET", path: "/agents/stats", rule: listed(coordinatorAudience, reached)},

	// ---- platform experiments ----
	{method: "POST", path: "/platform-experiments", body: pe1, rule: createsIn},
	{method: "GET", path: "/platform-experiments", rule: listed(agentAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}", request: "/platform-experiments/<pe>", rule: scoped(agentAudience, reached)},
	{method: "PUT", path: "/platform-experiments/{id}", request: "/platform-experiments/<pe>", body: pe1, rule: scoped(coordinatorAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/signups/{agent_id}", request: "/platform-experiments/<pe>/signups/<agent>", rule: scoped(agentAudience, reached)},
	{method: "POST", path: "/platform-experiments/{id}/start", request: "/platform-experiments/<pe>/start", rule: scoped(coordinatorAudience, reached)},
	{method: "POST", path: "/platform-experiments/{id}/close", request: "/platform-experiments/<pe>/close", rule: scoped(coordinatorAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/results", request: "/platform-experiments/<pe>/results", rule: scoped(agentAudience, reached)},
	{method: "POST", path: "/platform-experiments/{id}/summary", request: "/platform-experiments/<pe>/summary", body: `{"summary":"s"}`, rule: scoped(coordinatorAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/quotas", request: "/platform-experiments/<pe>/quotas", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/quotas/{agentID}", request: "/platform-experiments/<pe>/quotas/<agent>", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/stages", request: "/platform-experiments/<pe>/stages", rule: scoped(agentAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/metrics-timeseries", request: "/platform-experiments/<pe>/metrics-timeseries?metric_name=loss", rule: scoped(agentAudience, metricsUnreachable)},
	{method: "GET", path: "/platform-experiments/{id}/data-usage", request: "/platform-experiments/<pe>/data-usage", rule: scoped(agentAudience, reached)},
	{method: "POST", path: "/platform-experiments/{id}/data-key", request: "/platform-experiments/<pe>/data-key", body: `{"agent_id":"<agent>"}`, rule: spendsAs(owned(agentAudience, reached))},
	{method: "POST", path: "/platform-experiments/{id}/agents", request: "/platform-experiments/<pe>/agents", body: `{"agent_id":"<agent>"}`, rule: scoped(coordinatorAudience, reached)},
	{method: "GET", path: "/platform-experiments/{id}/agents", request: "/platform-experiments/<pe>/agents", rule: scoped(coordinatorAudience, reached)},
	{method: "DELETE", path: "/platform-experiments/{id}/agents/{agent_id}", request: "/platform-experiments/<pe>/agents/<agent>", rule: scoped(coordinatorAudience, reached)},

	// ---- donations ----
	{method: "GET", path: "/donations", request: "/donations?platform_experiment_id=<pe>", rule: listed(agentAudience, reached)},
	{method: "POST", path: "/donations", body: `{"agent_id":"<agent>","platform_experiment_id":"<pe>","credits_want":1,"reason":"r"}`, rule: spendsAs(owned(agentAudience, reached))},
	{method: "POST", path: "/donations/{id}/cancel", request: "/donations/<don>/cancel", rule: owned(agentAudience, reached)},
	{method: "POST", path: "/donations/{id}/fulfill", request: "/donations/<don>/fulfill", body: `{"donor_agent_id":"agent-1"}`, rule: spendsAs(scoped(agentAudience, reached))},

	// ---- catalog ----
	{method: "GET", path: "/resource-catalog", rule: listed(agentAudience, http.StatusOK)},
	{method: "GET", path: "/resource-catalog/capacity", rule: listed(agentAudience, metricsUnreachable)},

	// ---- accounts ----
	{method: "GET", path: "/me", rule: only(people, reached)},
	{method: "POST", path: "/orgs", body: `{"name":"n"}`, rule: only([]string{"user"}, reached)},
	{method: "PATCH", path: "/orgs/{id}", request: "/orgs/<org>", body: `{"name":"n"}`, rule: orgLevel(people, http.StatusForbidden, reached)},
	{method: "GET", path: "/orgs/{id}/projects", request: "/orgs/<org>/projects", rule: inOrg(reached)},
	{method: "POST", path: "/orgs/{id}/projects", request: "/orgs/<org>/projects", body: `{"name":"n"}`, rule: orgLevel(people, http.StatusForbidden, reached)},
	{method: "PATCH", path: "/projects/{id}", request: "/projects/<prj>", body: `{"name":"n"}`, rule: ofProject(reached)},
	{method: "POST", path: "/orgs/{id}/api-keys", request: "/orgs/<org>/api-keys", body: `{"name":"n"}`, rule: orgLevel(people, http.StatusForbidden, reached)},
	{method: "GET", path: "/orgs/{id}/api-keys", request: "/orgs/<org>/api-keys", rule: inOrg(reached)},
	{method: "DELETE", path: "/api-keys/{id}", request: "/api-keys/key-of-<org>", rule: orgLevel(people, http.StatusNotFound, reached)},
	{method: "POST", path: "/orgs/{id}/clusters", request: "/orgs/<org>/clusters", body: `{"name":"cluster-new"}`, rule: orgLevel(people, http.StatusForbidden, reached)},
	{method: "GET", path: "/orgs/{id}/clusters", request: "/orgs/<org>/clusters", rule: orgLevel(people, http.StatusForbidden, reached)},
	{method: "PATCH", path: "/orgs/{id}/clusters/{name}", request: "/orgs/<org>/clusters/<cluster>", body: `{"shared":true}`, rule: staffOnly},
	{method: "DELETE", path: "/orgs/{id}/clusters/{name}", request: "/orgs/<org>/clusters/<cluster>", rule: orgLevel(people, http.StatusForbidden, reached)},
	{method: "DELETE", path: "/orgs/{id}/clusters/{name}/identity", request: "/orgs/<org>/clusters/<cluster>/identity", rule: orgLevel(people, http.StatusForbidden, reached)},

	// ---- runtimes and launchers ----
	{method: "GET", path: "/internal/clusters", rule: staffOnly},
	{method: "POST", path: "/internal/clusters/{name}/reconcile", request: "/internal/clusters/<cluster>/reconcile", body: capacity1, rule: ofCluster(metricsUnreachable)},
	{method: "POST", path: "/internal/clusters/{name}/status", request: "/internal/clusters/<cluster>/status", body: `{"reports":[]}`, rule: ofCluster(reached)},
	{method: "POST", path: "/internal/launchers/{name}/reconcile", request: "/internal/launchers/main/reconcile", body: `{"launch_digest":"0123456789abcdef0123456789abcdef","instances":[],"reconcile_interval_seconds":15,"create_pause_milliseconds":1000}`, rule: only([]string{"launcher"}, reached)},
	{method: "GET", path: "/metrics", rule: staffOnly},
}

// inOrgOf is a write that names the org it lands in.
func inOrgOf(audience []string, admitted int) rule {
	return func(principal, owner string) int {
		if status, refused := gate(audience, principal); refused {
			return status
		}
		if owner == "foreign" || owner == "absent" {
			return http.StatusNotFound
		}
		return admitted
	}
}

// createsIn is the creation of a platform experiment in a project: a token already bound to one
// platform experiment creates no other, and a key bound to a project none outside it.
func createsIn(principal, owner string) int {
	if status, refused := gate(coordinatorAudience, principal); refused {
		return status
	}
	if principal == "coordinator" {
		return http.StatusForbidden
	}
	if !holds(principal, owner) {
		return http.StatusNotFound
	}
	return reached
}

// cases are requests the table's one request per route does not make: each is a way a caller
// once got past, or a second object a write names.
var cases = []struct {
	why, principal, method, target, body string
	want                                 int
}{
	{"an id that is a path into another org's data", "user", "GET", "/platform-experiments/pe-b%2Fagent-9/data-usage", "", 404},
	{"the same id from staff, who pass on an absent row", "staff", "GET", "/platform-experiments/pe-b%2Fagent-9/data-usage", "", 404},
	{"an id that is a wildcard", "staff", "GET", "/platform-experiments/pe-*/data-usage", "", 404},
	{"a data key under an id that is a path", "staff", "POST", "/platform-experiments/pe-a1%2F..%2Fpe-b/data-key", `{"agent_id":"agent-1"}`, 404},
	{"a job id that is a path", "staff", "GET", "/experiments/job-own%2F..%2Fjob-foreign/data", "", 404},
	{"an agent id that is a wildcard", "staff", "POST", "/platform-experiments/pe-a1/data-key", `{"agent_id":"agent-*"}`, 404},
	{"capacity of an org the person is not in", "user", "GET", "/resource-catalog/capacity?org_id=org-b", "", 404},
	{"capacity of another org with an org key", "org key", "GET", "/resource-catalog/capacity?org_id=org-b", "", 404},
	{"capacity of another org with a project key", "project key", "GET", "/resource-catalog/capacity?org_id=org-b", "", 404},
	{"capacity of another org with an agent token", "agent", "GET", "/resource-catalog/capacity?org_id=org-b", "", 404},
	{"capacity of another org with a coordinator token", "coordinator", "GET", "/resource-catalog/capacity?org_id=org-b", "", 404},
	{"capacity of an org that does not exist, from staff", "staff", "GET", "/resource-catalog/capacity?org_id=org-never", "", 404},
	{"capacity of the caller's own org, named", "user", "GET", "/resource-catalog/capacity?org_id=org-a", "", metricsUnreachable},
	{"capacity of one org, from staff", "staff", "GET", "/resource-catalog/capacity?org_id=org-b", "", metricsUnreachable},
	{"settings under a cluster id that is a path", "staff", "GET", "/clusters/id%2F..%2Fother/settings", "", 404},
	{"settings written under a cluster id no runtime could report", "staff", "PUT", "/clusters/id%20with%20spaces/settings", `{}`, 404},
	{"settings written under an overlong cluster id", "staff", "PUT", "/clusters/" + strings.Repeat("a", 200) + "/settings", `{}`, 404},

	{"an agent registering a hypothesis as a person", "agent", "POST", "/hypotheses", `{"author":"Mallory","platform_experiment_id":"pe-a1","text":"t","links":[{"type":"builds_on","to":"pe-a1"}]}`, 403},
	{"an agent registering as itself and a person", "agent", "POST", "/hypotheses", `{"agent_id":"agent-1","author":"Mallory","platform_experiment_id":"pe-a1","text":"t","links":[{"type":"builds_on","to":"pe-a1"}]}`, 403},
	{"an agent linking as a person", "agent", "POST", "/hypotheses/hyp-own/links", `{"author":"Mallory","type":"related","to":"hyp-peer"}`, 403},
	{"an agent commenting as a person", "agent", "POST", "/hypotheses/hyp-own/comments", `{"author":"Mallory","text":"t"}`, 403},
	{"an agent commenting as nobody", "agent", "POST", "/hypotheses/hyp-own/comments", `{"text":"t"}`, 403},
	{"a user commenting under their own name", "user", "POST", "/hypotheses/hyp-own/comments", `{"text":"t"}`, reached},
	{"a user commenting for an agent of another org", "user", "POST", "/hypotheses/hyp-own/comments", `{"agent_id":"agent-9","text":"t"}`, 404},
	{"a user commenting for an agent nobody registered", "user", "POST", "/hypotheses/hyp-own/comments", `{"agent_id":"agent-none","text":"t"}`, 404},

	{"an agent giving away another agent's quota", "agent", "POST", "/donations/don-peer/fulfill", `{"donor_agent_id":"agent-2"}`, 403},
	{"an agent submitting a job as another agent", "agent", "POST", "/experiments", `{"id":"job-new","metadata":{"platform_experiment_id":"pe-a1","agent_id":"agent-2","hypothesis_id":"hyp-own","code_ref":"r@0","config_hash":"h","data_ref":"d","objective":"o","theory":"t","estimated_duration_hours":1},"job":{"image":"i","cpu":"1","memory":"1Gi","storage":"1Gi","max_retries":0}}`, 403},
	{"a coordinator creating a second platform experiment in its own project", "coordinator", "POST", "/platform-experiments", `{"project_id":"prj-a1","name":"n","description":"d","budget_accelerator_hours":1,"max_agents":2,"metrics":[{"key":"loss","direction":"minimize","role":"ranking"}],"report_interval_seconds":60,"starts_at":"2030-01-01T00:00:00Z","ends_at":"2030-01-02T00:00:00Z"}`, 403},

	{"a link to a hypothesis of another org", "user", "POST", "/hypotheses/hyp-own/links", `{"agent_id":"agent-1","type":"related","to":"hyp-foreign"}`, 404},
	{"a link to a hypothesis that does not exist", "user", "POST", "/hypotheses/hyp-own/links", `{"agent_id":"agent-1","type":"related","to":"hyp-none"}`, 404},
	{"a hypothesis built on one of another org", "user", "POST", "/hypotheses", `{"agent_id":"agent-1","platform_experiment_id":"pe-a1","text":"t","links":[{"type":"builds_on","to":"hyp-foreign"}]}`, 404},
	{"a project key linking into the sibling project", "project key", "POST", "/hypotheses/hyp-own/links", `{"agent_id":"agent-1","type":"related","to":"hyp-sibling"}`, 404},

	{"a watch on another org's job", "user", "GET", "/watch?experiment_id=job-foreign", "", 404},
	{"a watch on a job that does not exist", "user", "GET", "/watch?experiment_id=job-none", "", 404},
	{"a watch on a job of the sibling project by a project key", "project key", "GET", "/watch?experiment_id=job-sibling", "", 404},
	{"a watch on the caller's own job", "agent", "GET", "/watch?experiment_id=job-own", "", reached},
	{"a watch with the token in the Authorization header", "user", "GET", "/watch-with-header", "", 401},

	{"a runtime reporting for a cluster of another org", "runtime", "POST", "/internal/clusters/cluster-b/status", `{"reports":[]}`, 403},
	{"a launcher asking as another launcher", "launcher", "POST", "/internal/launchers/other/reconcile", `{"instances":[]}`, 403},
	{"staff reading the process metrics", "staff", "GET", "/metrics", "", 200},
}
