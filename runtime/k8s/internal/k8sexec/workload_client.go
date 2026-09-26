// Package k8sexec provides a Kubernetes workload client for HypothesisLoop,
// using only native Kubernetes primitives — no external queueing operator.
//
// Admission (deciding whether a job fits available capacity) is decided entirely by the Go
// scheduler loop (services/scheduler) before this client is ever asked to create a Job, so
// there's no suspend/admit handshake here. This client just creates a plain batchv1.Job with a
// native scheduling.k8s.io/v1 PriorityClass so the k8s scheduler orders/preempts pods
// in tier order. The two priority classes and the namespace are the chart's (runtime/k8s/infra/chart).
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

	// The PriorityClass a job's pods carry, by capacity tier. The chart installs both, and
	// says there whether a pod of one may evict a pod of lower priority.
	PriorityClassGuaranteed = "hypothesisloop-jobs-guaranteed"
	PriorityClassBurst      = "hypothesisloop-jobs-burst"

	// DistributedMasterPort is the fixed rendezvous port advertised to every rank of a
	// distributed (NumNodes > 1) job via HYPOTHESISLOOP_MASTER_PORT — fine as a fixed port since
	// each job gets its own pods/Service, so no cross-job collision is possible.
	DistributedMasterPort = 29500
)

type Config struct {
	KubeconfigPath string
	KubeContext    string
	APIURL         string
	// JobImage is the image a job that names none runs in. Empty on a cluster that takes only
	// jobs that name their image.
	JobImage string
}

type JobWorkloadClient struct {
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	apiURL string
	// jobImage is reported to the control plane, which then places here the jobs that name no
	// image, or name this one.
	jobImage string
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
	return &JobWorkloadClient{kube: kube, dyn: dyn, apiURL: cfg.APIURL, jobImage: cfg.JobImage}, nil
}
