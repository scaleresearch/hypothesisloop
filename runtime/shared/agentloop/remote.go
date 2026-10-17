package agentloop

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

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
