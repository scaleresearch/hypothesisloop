package httpapi

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

type clusterList struct {
	Body struct {
		Clusters []accounts.ListedCluster `json:"clusters"`
	}
}

func listOfClusters(clusters []accounts.ListedCluster, err error) (*clusterList, error) {
	if err != nil {
		return nil, err
	}
	out := &clusterList{}
	out.Body.Clusters = clusters
	return out, nil
}

// clusterUseDescription ends the description of turning a cluster off and on for an org.
const clusterUseDescription = "The cluster is any the org can use, its own or one the platform shares, and the decision is the org's alone: it changes nothing for another org. " +
	"An owner of the org, an API key of the whole org and staff decide it. Repeating it changes nothing. A cluster the org cannot use is not found."

func (a accountsAPI) registerClusters(doc *apidocs.Doc) {
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "register-cluster", Method: "POST", Path: "/orgs/{id}/clusters",
		Summary: "Register a cluster for an org", Tags: []string{"clusters"},
		Description: "A cluster exists for the platform once its name is registered, and takes the jobs of its org only. " +
			"Returns what its agents are started with: the runtime token, the two addresses they reach the platform at, and attach_commands, the complete command that starts them on a Kubernetes cluster and on a bare-metal node. " +
			"The token is derived from the name, so it is never lost: registering a name the org already owns changes nothing " +
			"and returns the same token and addresses. A name another org owns answers 409.",
	}, a.registerCluster)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "list-clusters", Method: "GET", Path: "/orgs/{id}/clusters",
		Summary: "List the clusters an org's jobs run on", Tags: []string{"clusters"},
		Description: "The clusters the org registered and the ones the platform shares with every org, in the order a job is placed on them, with whether an agent is connected right now " +
			"and whether the org has the cluster on for its jobs (enabled). The org may change only the ones it owns, and turns any of them off or on for its own jobs.",
	}, a.listClusters)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "order-clusters", Method: "PUT", Path: "/orgs/{id}/clusters/order",
		Summary: "Set the order an org's jobs are placed on its clusters", Tags: []string{"clusters"},
		Description: "names is every cluster the org registered and the platform does not share, each once, first placed first: anything else answers 422 and changes nothing. " +
			"A job is placed on the first cluster that can take it as the scheduler reads capacity at that moment, and on a later one when that is full or has just failed it. " +
			"The order holds for placements from then on and moves no job that is placed. A cluster registered afterwards comes last until it is ordered, " +
			"and the clusters the platform shares come after every one of the org's own. Answers the clusters as the list does, in the new order.",
	}, a.orderClusters)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "disable-cluster", Method: "POST", Path: "/orgs/{id}/clusters/{name}/disable",
		Summary: "Turn a cluster off for an org's jobs", Tags: []string{"clusters"},
		Description: "From here on no job of the org is placed on the cluster, and the capacity the org reads leaves it out. " +
			"The org's jobs already placed there are not touched: they run to their end. A queued job of an org that turned off every cluster " +
			"it could use waits with not_admitted_reason clusters_disabled. " + clusterUseDescription,
	}, a.disableCluster)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "enable-cluster", Method: "POST", Path: "/orgs/{id}/clusters/{name}/enable",
		Summary: "Turn a cluster on again for an org's jobs", Tags: []string{"clusters"},
		Description: "The org's jobs are placed on the cluster again, which is how every cluster starts. " + clusterUseDescription,
	}, a.enableCluster)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "update-cluster", Method: "PATCH", Path: "/orgs/{id}/clusters/{name}",
		Summary: "Change a cluster: its scheduler settings, or whether every org shares it", Tags: []string{"clusters"},
		Description: "Takes the fields that change, at least one. " +
			"shared is staff only. A shared cluster takes jobs of every org. When the sharing ends, the jobs of other orgs placed on the cluster " +
			"go back to the queue in the same step and are placed elsewhere: the cluster's runtime stops them, giving each its checkpoint window, " +
			"and none of them spends a retry. requeued names them. The cluster then comes last among its org's own until the org orders them. " +
			"scale_up_timeout_seconds, max_speculative_accelerators and stuck_pending_timeout_seconds are the cluster's own values in place of the scheduler's defaults, " +
			"for a cluster whose nodes appear when a job asks for them, and status_silence_ceiling_seconds how long the cluster may report no job status before the jobs it holds are evicted as cluster_unreachable: " +
			"each takes a value, or 0 to go back to the default.",
	}, a.updateCluster)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "remove-cluster", Method: "DELETE", Path: "/orgs/{id}/clusters/{name}",
		Summary: "Remove a cluster from an org", Tags: []string{"clusters"},
		Description: "Ends the cluster for the platform. Every job placed on it goes back to the queue in the same step, keeping its retries, and is placed " +
			"on another cluster that takes its org. The cluster's settings and the identity bound to it are deleted, and its runtime token is refused from " +
			"then on, so stop its agents. The name is free to register again, by any org. requeued names the jobs.",
	}, a.removeCluster)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "forget-cluster-identity", Method: "DELETE", Path: "/orgs/{id}/clusters/{name}/identity",
		Summary: "Forget the identity a cluster's runtime reported", Tags: []string{"clusters"},
		Description: "A cluster is bound to the first cluster_id its runtime reports, and a runtime reporting any other is refused. After the machines " +
			"behind a name are rebuilt they report a new one: forget the old identity here and the cluster binds to what its runtime reports next. " +
			"The cluster keeps its name, priority and settings.",
	}, a.forgetClusterIdentity)
}

func (a accountsAPI) registerCluster(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body nameBody
}) (*struct{ Body accounts.RegisteredCluster }, error) {
	registered, err := a.accounts.RegisterCluster(ctx, in.ID, in.Body.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.RegisteredCluster }{Body: *registered}, nil
}

func (a accountsAPI) listClusters(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*clusterList, error) {
	return listOfClusters(a.accounts.Clusters(ctx, in.ID))
}

func (a accountsAPI) orderClusters(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Names []string `json:"names" doc:"The org's own clusters, first placed first."`
	}
}) (*clusterList, error) {
	return listOfClusters(a.accounts.OrderClusters(ctx, in.ID, in.Body.Names))
}

func (a accountsAPI) disableCluster(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}) (*struct{ Body accounts.ClusterView }, error) {
	return a.setClusterEnabled(ctx, in.ID, in.Name, false)
}

func (a accountsAPI) enableCluster(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}) (*struct{ Body accounts.ClusterView }, error) {
	return a.setClusterEnabled(ctx, in.ID, in.Name, true)
}

func (a accountsAPI) setClusterEnabled(ctx context.Context, orgID, name string, enabled bool) (*struct{ Body accounts.ClusterView }, error) {
	view, err := a.accounts.SetClusterEnabled(ctx, orgID, name, enabled)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.ClusterView }{Body: *view}, nil
}

func (a accountsAPI) updateCluster(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Name string `path:"name"`
	Body struct {
		Shared *bool `json:"shared,omitempty" doc:"Staff only."`
		domain.ClusterSettings
	}
}) (*struct{ Body accounts.WithdrawnCluster }, error) {
	withdrawn, err := a.accounts.UpdateCluster(ctx, in.ID, in.Name, in.Body.Shared, in.Body.ClusterSettings)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.WithdrawnCluster }{Body: *withdrawn}, nil
}

func (a accountsAPI) removeCluster(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}) (*struct{ Body accounts.WithdrawnCluster }, error) {
	withdrawn, err := a.accounts.RemoveCluster(ctx, in.ID, in.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.WithdrawnCluster }{Body: *withdrawn}, nil
}

func (a accountsAPI) forgetClusterIdentity(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}) (*struct{ Body accounts.ClusterView }, error) {
	view, err := a.accounts.ForgetClusterIdentity(ctx, in.ID, in.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.ClusterView }{Body: *view}, nil
}
