package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// inClusterPodLister builds a clientset from the pod's own in-cluster service account
// credentials (the standard kubelet-mounted token/CA, automatic when running inside the
// cluster with no kubeconfig given) — this daemonset only ever needs to read pods on its own
// node, so no other credential source is supported.
func inClusterPodLister() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return cs, nil
}

// podExperimentIDs lists every pod on this node in the experiment-jobs namespace and returns
// pod UID -> hypothesisloop.io/experiment-id label value, for pods that have it set. Identity
// comes from this label, set once by the control plane at pod creation
// (workload_client.go) — never inferred from anything the pod itself reports, so it can't be
// spoofed or drift out of sync with what actually admitted the job.
func podExperimentIDs(ctx context.Context, cs kubernetes.Interface, nodeName string) (map[string]string, error) {
	list, err := cs.CoreV1().Pods(jobsNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list.Items))
	for _, pod := range list.Items {
		if id := pod.Labels[experimentIDLabel]; id != "" {
			out[string(pod.UID)] = id
		}
	}
	return out, nil
}
