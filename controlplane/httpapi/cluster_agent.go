package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/clusterexchange"
)

type reconcileInput struct {
	Name string `path:"name"`
	// AgentVersion is clusterapi.AgentVersionHeader's value.
	AgentVersion string `header:"X-Hypothesisloop-Agent-Version"`
	Body         clusterapi.ReconcileRequest
}

type clusterAgentAPI struct {
	exchange *clusterexchange.Service
	logger   *zap.Logger
}

// DetachAnswer is the control plane's answer to a runtime ending its own cluster
// (DELETE /internal/clusters/{name}): the jobs that went back to the queue.
type DetachAnswer struct {
	Requeued []string `json:"requeued"`
}

// RegisterClusterAgent registers the cluster-agent-facing operations on doc. Paths are relative
// to the /internal/clusters mount. Tagged "cluster-agent", a separate consumer from the research
// agent, so it gets its own OpenAPI doc and explore.
func RegisterClusterAgent(doc *Doc, exchange *clusterexchange.Service) {
	a := clusterAgentAPI{exchange: exchange, logger: doc.logger}
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "list-clusters", Method: "GET", Path: "/",
		Summary: "List clusters and their connectivity", Tags: []string{"cluster-agent"},
		Description: "Every cluster that has ever polled, and whether its agent is connected right now.",
	}, a.list)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-access", Method: "GET", Path: "/{name}" + clusterapi.AccessPath, DefaultStatus: http.StatusNoContent,
		Summary: "Check this cluster's runtime token", Tags: []string{"cluster-agent"},
		Description: "Answers 204 to the runtime token of a registered cluster named name, and refuses any other token as every route here does. It records and changes nothing, so an installer's dry run asks it before anything is installed.",
	}, a.access)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-reconcile", Method: "POST", Path: "/{name}" + clusterapi.ReconcilePath,
		MaxBodyBytes: clusterexchange.MaxReconcileBytes,
		Summary:      "Exchange actual capacity for desired workloads", Tags: []string{"cluster-agent"},
		Description: "Atomically records one complete actual-capacity snapshot in metrics storage, keeping only priced accelerator types, then returns the complete PostgreSQL desired workload set for this cluster, plus the ids of workloads whose termination earns them their declared checkpoint window. " +
			"Each workload names the image it runs in by digest, with image_pull, a read-only login that pulls it, when the image needs one, and the launch script every node runs. " +
			"The version the agent sends in " + clusterapi.AgentVersionHeader + " is kept with the snapshot, and a refusal of the request is kept as the cluster's last error until one is accepted.",
	}, a.reconcile)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-detach", Method: "DELETE", Path: "/{name}",
		Summary: "Detach this cluster", Tags: []string{"cluster-agent"},
		Description: "The cluster's own runtime ends the cluster, as its owner does with DELETE /orgs/{id}/clusters/{name}: every job placed on it goes back to the queue, " +
			"its settings and the identity bound to it are deleted, and its runtime token is refused from then on with reason cluster_not_registered. requeued names the jobs.",
	}, a.detach)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-push-status", Method: "POST", Path: "/{name}" + clusterapi.StatusPath,
		Summary: "Push job-phase observations from a cluster-agent", Tags: []string{"cluster-agent"},
		Description: "Body: {\"reports\": [...]}. The complete snapshot is validated and written atomically; any invalid report rejects the whole request.",
	}, a.pushStatus)
}

type allClusters struct {
	Clusters []clusterexchange.ClusterInfo `json:"clusters"`
}

func (a clusterAgentAPI) list(ctx context.Context, _ *struct{}) (*struct{ Body allClusters }, error) {
	clusters, err := a.exchange.Clusters(ctx)
	if err != nil {
		return nil, err
	}
	return &struct{ Body allClusters }{Body: allClusters{Clusters: clusters}}, nil
}

func (a clusterAgentAPI) access(ctx context.Context, in *struct {
	Name string `path:"name"`
}) (*struct{}, error) {
	return nil, nil
}

func (a clusterAgentAPI) reconcile(ctx context.Context, in *reconcileInput) (*struct {
	Body clusterapi.ReconcileAnswer
}, error) {
	answer, err := a.exchange.Reconcile(ctx, in.Name, in.AgentVersion, in.Body)
	if err != nil {
		return nil, refused(a.logger, "cluster API", err)
	}
	return &struct {
		Body clusterapi.ReconcileAnswer
	}{Body: answer}, nil
}

func (a clusterAgentAPI) detach(ctx context.Context, in *struct {
	Name string `path:"name"`
}) (*struct {
	Body DetachAnswer
}, error) {
	requeued, err := a.exchange.Detach(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	return &struct {
		Body DetachAnswer
	}{Body: DetachAnswer{Requeued: requeued}}, nil
}

func (a clusterAgentAPI) pushStatus(ctx context.Context, in *struct {
	Name string `path:"name"`
	Body clusterapi.StatusPush
}) (*struct {
	Body clusterapi.StatusAck
}, error) {
	if err := a.exchange.PushStatus(ctx, in.Name, in.Body); err != nil {
		return nil, refused(a.logger, "cluster API", err)
	}
	return &struct {
		Body clusterapi.StatusAck
	}{Body: clusterapi.StatusAck{Status: "ok"}}, nil
}
