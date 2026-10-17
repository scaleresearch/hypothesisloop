package registry

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/storage/graphdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/objectstore"
)

// QueryResult is what a read-only query of the hypothesis graph answers.
type QueryResult = graphdb.QueryResult

// HypothesisFilter narrows a listing of hypotheses.
type HypothesisFilter = graphdb.ListFilter

// DataObject is one object a job left in the durable-data store.
type DataObject = objectstore.Object

// ErrInvalidQuery refuses a query the graph does not run, with why.
var ErrInvalidQuery = graphdb.ErrInvalidQuery

// QueryDescription is the schema an agent needs to write Cypher against the graph. It is served
// at /explore; the instructions the platform serves an agent carry only the rules, as /explore/rules.
var QueryDescription = "Read-only Cypher against one platform experiment's hypothesis graph (Memgraph). " +
	"Body {platform_experiment_id, cypher, params}; answer " +
	"{columns, rows, truncated}. Use it to find where an idea belongs before registering it, and to walk lineage. " +
	"Schema: (:Objective {id}) is the root of one platform experiment, id = platform_experiment_id. " +
	"(:Hypothesis {id, platform_experiment_id, agent_id, actor_kind, actor_id, text, status, created_at, status_changed_at, " +
	"status_actor_kind, status_actor_id}). agent_id is whose hypothesis it is, empty on one no agent owns. actor_kind " +
	"(user, key, agent, coordinator, staff) and actor_id are the credential that registered it, and status_actor_* the one " +
	"that gave the verdict; a node from before they were kept has neither, and author holds the name it stated. " +
	"created_at and status_changed_at are datetimes, returned as RFC3339 text: compare them with " +
	"datetime('2026-10-02T01:30:00Z'), since a string never matches one. " +
	"Relationships, each {agent_id, actor_kind, actor_id, created_at}: (:Hypothesis)-[:BUILDS_ON]->(:Hypothesis or :Objective) " +
	"(refines, extends, combines; acyclic, every hypothesis has one or more), (:Hypothesis)-[:CONTRADICTS]->(:Hypothesis), " +
	"(:Hypothesis)-[:RELATED]->(:Hypothesis). A query reads this platform experiment and nothing else, whatever it returns: " +
	"the platform rewrites it before it runs. Every node pattern names its label, (h:Hypothesis) or (o:Objective), and is " +
	"tied to $pe (platform_experiment_id on a hypothesis, id on the objective), which you may leave out or write as " +
	"{platform_experiment_id: $pe} and {id: $pe}; a variable-length relationship ([:BUILDS_ON*1..3]) stays inside the " +
	"platform experiment and takes no filter of its own; a text search keeps this platform experiment's hits. Refused with 422 naming cypher: " +
	"a node without a label, as (n), () or a variable in parentheses on its own, which is then written (n:Hypothesis) " +
	"again, another or a second label, the scope property set to anything but $pe, a parameter as a property map, a " +
	"function outside the built-in ones (" + strings.Join(graphdb.QueryFunctions, ", ") + "), a statement that does not start with " +
	"MATCH, OPTIONAL MATCH, WITH, UNWIND, RETURN or CALL, and a CALL without YIELD node. " +
	"$evidence is bound to a map by hypothesis id of what is outside the graph: {jobs (rejected aside), submitted, queued, " +
	"running, completed, failed, evicted, rejected, findings, comments} as counts and last_finding_at (datetime). A hypothesis with none " +
	"of these has no entry and reads as null: `WHERE coalesce($evidence[h.id].jobs, 0) = 0` finds the untested ones. " +
	"$evidence[$pe] is the objective's: the baseline's jobs and the comments on the objective. " +
	"Full-text index " + graphdb.TextIndex + " over Hypothesis.text, ranked by how many of your words match: " +
	"`CALL text_search.search_all('" + graphdb.TextIndex + "', 'warmup cosine schedule') YIELD node, score " +
	"RETURN node.id, node.text, node.status, score ORDER BY score DESC LIMIT 10` " +
	"(plain words; quotes, colons and parentheses are search syntax). " +
	"Children of a hypothesis: `MATCH (c:Hypothesis)-[:BUILDS_ON]->(:Hypothesis {id: $id}) RETURN c.id, c.text, c.status`. " +
	"Nodes come back as their properties plus _labels, relationships as properties plus _type, _from and _to, paths as lists. " +
	"Nothing a query writes persists (writes go through POST /hypotheses and POST /hypotheses/{id}/links). " +
	"LOAD CSV, periodic commits and procedures other than " + strings.Join(graphdb.QueryProcedures, ", ") + " are refused. Limits: " +
	fmt.Sprintf("%s and %d MB of memory per query, %d rows (truncated=true when cut). ", graphdb.QueryTimeout, graphdb.QueryMemoryLimitMB, graphdb.QueryRowCap) +
	"A Cypher error comes back as 422 naming cypher, with Memgraph's message. Findings, comments and jobs are not in the graph: " +
	"GET /hypotheses/{id} returns them, and GET /hypotheses/{platform_experiment_id}/comments the objective's comments."
