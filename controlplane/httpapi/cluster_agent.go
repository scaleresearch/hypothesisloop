package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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

// buildPath is the path of one build of a cluster.
const buildPath = "/{name}" + clusterapi.BuildsPath + "/{key}"

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
		Description: "Atomically records one complete actual-capacity snapshot in metrics storage, keeping only priced accelerator types, then returns the complete PostgreSQL desired workload set for this cluster, plus the ids of workloads whose termination earns them their declared checkpoint window, " +
			"the environment builds assigned to this cluster (builds, recorded as building only for a runtime that reports builds true), and registry_pull, a login that reads environment images, when a listed workload runs in one. " +
			"The version the agent sends in " + clusterapi.AgentVersionHeader + " is kept with the snapshot, and a refusal of the request is kept as the cluster's last error until one is accepted.",
	}, a.reconcile)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-credential", Method: "POST", Path: buildPath + clusterapi.BuildCredentialPath,
		Summary: "The push login of an assigned environment build", Tags: []string{"cluster-agent"},
		Description: "A login that writes to the environment repository for at least 25 minutes, handed out only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, a.buildCredential)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-git-credential", Method: "POST", Path: buildPath + clusterapi.BuildGitCredentialPath,
		Summary: "A read of the repository an assigned environment build is of", Tags: []string{"cluster-agent"},
		Description: "The git proxy address the repository whose environment file this is is read at, the repository's own address, and a platform token " +
			"that reads that repository alone through the proxy, read-only, while the build of key is assigned to this cluster at attempt and still building. " +
			"The build mounts them as the git config secret " + domain.EnvironmentGitSecret + " of a RUN that asks for it, never into a layer. Handed out only while the " +
			"build is assigned and building; otherwise 409. 404 for an environment of no repository: the build then mounts no secret.",
	}, a.buildGitCredential)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-report", Method: "POST", Path: buildPath,
		Summary: "Report the outcome of an assigned environment build", Tags: []string{"cluster-agent"},
		Description: "outcome pushed makes the environment ready in the image the repository holds under digest; file fails it with log_tail as the file's fault; " +
			"platform frees the build to be assigned again. Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409. " +
			"file is the exception: the file is the key's, so it fails a build still building at attempt or a later one, wherever it was assigned again.",
	}, a.buildReport)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-loaded", Method: "POST", Path: buildPath + clusterapi.BuildLoadedPath,
		Summary: "Report an assigned environment build held on one node", Tags: []string{"cluster-agent"},
		Description: "The image of the build, the repository's under digest, is in the image store of node while its push goes on: a job that waits for the environment and fits on that node is placed there, pinned to it, " +
			"and runs the image from there before it is pushed. The outcome is still reported once the push ends. " +
			"Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, a.buildLoaded)
	register(doc, auth.AudienceInternal, huma.Operation{
		OperationID: "cluster-build-progress", Method: "POST", Path: buildPath + clusterapi.BuildProgressPath,
		Summary: "Report the step an assigned environment build is at", Tags: []string{"cluster-agent"},
		Description: "step, one line of at most " + strconv.Itoa(domain.MaxEnvironmentProgressBytes) + " bytes, replaces the last one reported, and the jobs waiting for the environment show it. " +
			"Recorded only while the build of key is assigned to this cluster at attempt and still building; otherwise 409.",
	}, a.buildProgress)
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

func (a clusterAgentAPI) buildCredential(ctx context.Context, in *struct {
	Name string `path:"name"`
	Key  string `path:"key"`
	Body domain.EnvironmentBuildCredentialRequest
}) (*struct {
	Body domain.RegistryLogin
}, error) {
	login, err := a.exchange.BuildCredential(ctx, in.Name, in.Key, in.Body.Attempt)
	if err != nil {
		return nil, refused(a.logger, "cluster API", err)
	}
	return &struct {
		Body domain.RegistryLogin
	}{Body: login}, nil
}

func (a clusterAgentAPI) buildGitCredential(ctx context.Context, in *struct {
	Name string `path:"name"`
	Key  string `path:"key"`
	Body domain.EnvironmentBuildCredentialRequest
}) (*struct {
	Body domain.EnvironmentGitCredential
}, error) {
	credential, err := a.exchange.BuildGitCredential(ctx, in.Name, in.Key, in.Body.Attempt)
	if err != nil {
		return nil, refused(a.logger, "cluster API", err)
	}
	return &struct {
		Body domain.EnvironmentGitCredential
	}{Body: credential}, nil
}

func (a clusterAgentAPI) buildReport(ctx context.Context, in *struct {
	Name string `path:"name"`
	Key  string `path:"key"`
	Body domain.EnvironmentBuildReport
}) (*struct{}, error) {
	return nil, refused(a.logger, "cluster API", a.exchange.ReportBuild(ctx, in.Name, in.Key, in.Body))
}

func (a clusterAgentAPI) buildLoaded(ctx context.Context, in *struct {
	Name string `path:"name"`
	Key  string `path:"key"`
	Body domain.EnvironmentBuildLoaded
}) (*struct{}, error) {
	return nil, refused(a.logger, "cluster API", a.exchange.BuildLoaded(ctx, in.Name, in.Key, in.Body))
}

func (a clusterAgentAPI) buildProgress(ctx context.Context, in *struct {
	Name string `path:"name"`
	Key  string `path:"key"`
	Body domain.EnvironmentBuildProgress
}) (*struct{}, error) {
	return nil, refused(a.logger, "cluster API", a.exchange.BuildProgress(ctx, in.Name, in.Key, in.Body))
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
