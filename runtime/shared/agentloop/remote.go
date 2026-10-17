package agentloop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

// BuildAPI is what a cluster that builds environments asks of the control plane about one
// assigned build at its attempt.
type BuildAPI interface {
	BuildLoaded(ctx context.Context, key string, loaded clusterapi.EnvironmentBuildLoaded) error
	BuildCredential(ctx context.Context, key string, attempt int) (clusterapi.RegistryLogin, error)
	BuildGitCredential(ctx context.Context, key string, attempt int) (clusterapi.EnvironmentGitCredential, error)
	ReportBuild(ctx context.Context, key string, report clusterapi.EnvironmentBuildReport) error
	BuildProgress(ctx context.Context, key string, progress clusterapi.EnvironmentBuildProgress) error
}

// Remote is a cluster's API on a control plane it reaches over HTTP with its runtime token.
type Remote struct {
	ClusterName string
	APIURL      string
	// Token is this cluster's runtime token, sent on every call to the control plane.
	Token string
	// Version is what the agent runs, its image tag, sent on every call beside the token
	// (clusterapi.AgentVersionHeader). Empty sends none.
	Version string
	HTTP    *http.Client
}

func (r *Remote) client() *cpclient.Client {
	client := &cpclient.Client{BaseURL: r.APIURL + clusterapi.ClustersPath + "/" + r.ClusterName, Token: r.Token, HTTP: r.HTTP}
	if r.Version != "" {
		client.Header = http.Header{clusterapi.AgentVersionHeader: {r.Version}}
	}
	return client
}

// call makes one request of this cluster's API. A 401 that says the cluster is no longer
// registered is ErrClusterRemoved; any other refusal is the control plane's own words, and the
// loop goes on either way: a 401 or 403 is a token or a registration somebody has to fix, and the
// reason is what tells them which.
func (r *Remote) call(ctx context.Context, method, path string, body, out any) error {
	err := r.client().Do(ctx, method, path, body, out)
	var refused *cpclient.Refusal
	if errors.As(err, &refused) && refused.Status == http.StatusUnauthorized && refused.Reason == clusterapi.ReasonClusterNotRegistered {
		return ErrClusterRemoved
	}
	return err
}

func (r *Remote) Reconcile(ctx context.Context, report clusterapi.ReconcileRequest) (clusterapi.ReconcileAnswer, error) {
	var answer clusterapi.ReconcileAnswer
	if err := r.call(ctx, http.MethodPost, clusterapi.ReconcilePath, report, &answer); err != nil {
		return clusterapi.ReconcileAnswer{}, err
	}
	return answer, nil
}

func (r *Remote) PushStatus(ctx context.Context, push clusterapi.StatusPush) error {
	var ack clusterapi.StatusAck
	if err := r.call(ctx, http.MethodPost, clusterapi.StatusPath, push, &ack); err != nil {
		return err
	}
	if ack.Status != "ok" {
		return fmt.Errorf("invalid acknowledgement status %q", ack.Status)
	}
	return nil
}

func buildPath(key string) string { return clusterapi.BuildsPath + "/" + url.PathEscape(key) }

func (r *Remote) BuildLoaded(ctx context.Context, key string, loaded clusterapi.EnvironmentBuildLoaded) error {
	return r.call(ctx, http.MethodPost, buildPath(key)+clusterapi.BuildLoadedPath, loaded, nil)
}

func (r *Remote) BuildCredential(ctx context.Context, key string, attempt int) (clusterapi.RegistryLogin, error) {
	var login clusterapi.RegistryLogin
	err := r.call(ctx, http.MethodPost, buildPath(key)+clusterapi.BuildCredentialPath, clusterapi.EnvironmentBuildCredentialRequest{Attempt: attempt}, &login)
	return login, err
}

// BuildGitCredential answers agentexec.ErrNoEnvironmentGitCredential for a build whose file
// reads no repository.
func (r *Remote) BuildGitCredential(ctx context.Context, key string, attempt int) (clusterapi.EnvironmentGitCredential, error) {
	var credential clusterapi.EnvironmentGitCredential
	err := r.call(ctx, http.MethodPost, buildPath(key)+clusterapi.BuildGitCredentialPath, clusterapi.EnvironmentBuildCredentialRequest{Attempt: attempt}, &credential)
	var refused *cpclient.Refusal
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		return credential, fmt.Errorf("%w: %w", agentexec.ErrNoEnvironmentGitCredential, err)
	}
	return credential, err
}

func (r *Remote) ReportBuild(ctx context.Context, key string, report clusterapi.EnvironmentBuildReport) error {
	return r.call(ctx, http.MethodPost, buildPath(key), report, nil)
}

func (r *Remote) BuildProgress(ctx context.Context, key string, progress clusterapi.EnvironmentBuildProgress) error {
	return r.call(ctx, http.MethodPost, buildPath(key)+clusterapi.BuildProgressPath, progress, nil)
}

// CheckToken asks the control plane whether it takes this cluster's token, and changes nothing
// there: no capacity is reported and no identity bound. An installer's dry run asks it.
func (r *Remote) CheckToken(ctx context.Context) error {
	return r.call(ctx, http.MethodGet, clusterapi.AccessPath, nil, nil)
}

// SayIfStillRegistered tells, through log, how to remove this cluster from the control plane
// when it still takes the cluster's token.
func (r *Remote) SayIfStillRegistered(ctx context.Context, log func(format string, args ...any)) {
	err := r.CheckToken(ctx)
	switch {
	case errors.Is(err, ErrClusterRemoved):
		log("cluster %s is not registered at %s", r.ClusterName, r.APIURL)
	case err != nil:
		log("could not ask %s whether cluster %s is still registered (%v): if it is, remove it in the UI or with hl cluster remove %s", r.APIURL, r.ClusterName, err, r.ClusterName)
	default:
		log("cluster %s is still registered at %s: remove it in the UI or with hl cluster remove %s", r.ClusterName, r.APIURL, r.ClusterName)
	}
}
