// Package k8sexec provides a Kubernetes workload client for HypothesisLoop,
// using only native Kubernetes primitives — no external queueing operator.
//
// Admission (deciding whether a job fits available capacity) is decided entirely by the Go
// scheduler loop (services/scheduler) before this client is ever asked to create a Job, so
// there's no suspend/admit handshake here. This client just creates a plain batchv1.Job with a
// native scheduling.k8s.io/v1 PriorityClass so the k8s scheduler orders/preempts pods
// correctly under node pressure. Two priority classes exist: hypothesisloop-guaranteed, hypothesisloop-burst.
//
// This client belongs exclusively to cluster-agent. The control plane exposes desired state
// and never owns Kubernetes credentials.
//
// File layout: this file holds Config/New(); cluster_setup.go holds namespace/PriorityClass
// bootstrap and flavor-capacity config; job_lifecycle.go holds create/delete/poll operations;
// job_build.go holds BuildJob and its affinity/toleration helpers; util.go holds small shared helpers.
package k8sexec

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	HypothesisLoopNamespace = "hypothesisloop-jobs"

	// Native scheduling.k8s.io/v1 PriorityClass names. Higher value preempts lower
	// value at the k8s scheduler level under node resource pressure.
	PriorityClassGuaranteed = "hypothesisloop-guaranteed"
	PriorityClassBurst      = "hypothesisloop-burst"
	priorityValueGuaranteed = int32(1000000)
	priorityValueBurst      = int32(100000)

	// DistributedMasterPort is the fixed rendezvous port advertised to every rank of a
	// distributed (NumNodes > 1) job via HYPOTHESISLOOP_MASTER_PORT — fine as a fixed port since
	// each job gets its own pods/Service, so no cross-job collision is possible.
	DistributedMasterPort = 29500
)

type Config struct {
	KubeconfigPath string
	KubeContext    string
	APIURL         string
}

type JobWorkloadClient struct {
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	apiURL string
}

func New(cfg Config) (*JobWorkloadClient, error) {
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("workload: APIURL is required")
	}
	restCfg, err := buildRestConfig(cfg.KubeconfigPath, cfg.KubeContext)
	if err != nil {
		return nil, fmt.Errorf("workload: rest config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("workload: kube client: %w", err)
	}
	// dyn talks to resource.k8s.io as unstructured JSON rather than a generated typed client
	// pinned to one DRA API version — deliberate: k8s.io/client-go v0.31 (this repo's pin) only
	// ships resource.k8s.io/v1alpha3 types, but a modern cluster (e.g. k3s 1.36, GA DRA) serves
	// v1. dynamic.Interface lets BuildJob's DRA path work against whatever DRA API version the
	// target cluster runs, decoupled from this binary's compiled-in k8s.io/api version.
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("workload: dynamic client: %w", err)
	}
	return &JobWorkloadClient{kube: kube, dyn: dyn, apiURL: cfg.APIURL}, nil
}
