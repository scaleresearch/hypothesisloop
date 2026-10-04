package k8sexec

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// The agent holds no right to create a namespace or a PriorityClass, so it creates nothing at
// start and names what the chart did not install.
func TestSetupClusterNamesWhatTheChartDidNotInstallAndCreatesNothing(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: HypothesisLoopNamespace}}
	guaranteed := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: PriorityClassGuaranteed}}
	burst := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: PriorityClassBurst}}
	for name, tc := range map[string]struct {
		installed []runtime.Object
		missing   string
	}{
		"everything installed":   {[]runtime.Object{namespace, guaranteed, burst}, ""},
		"no namespace":           {[]runtime.Object{guaranteed, burst}, HypothesisLoopNamespace},
		"no burst PriorityClass": {[]runtime.Object{namespace, guaranteed}, PriorityClassBurst},
	} {
		kube := chartKube(t, tc.installed...)
		err := (&JobWorkloadClient{kube: kube}).SetupCluster(context.Background())
		if tc.missing == "" && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if tc.missing != "" && (err == nil || !strings.Contains(err.Error(), tc.missing)) {
			t.Errorf("%s: err = %v, want one naming %s", name, err, tc.missing)
		}
		for _, action := range kube.Actions() {
			if action.GetVerb() != "get" {
				t.Errorf("%s: the agent did %s %s at start", name, action.GetVerb(), action.GetResource().Resource)
			}
		}
	}
}

// A fresh pod can start before its network reaches the API server. The agent waits for it at
// start instead of exiting into a restart.
func TestSetupClusterWaitsForAnAPIServerItCannotReachYet(t *testing.T) {
	defer func(interval time.Duration) { setupRetryInterval = interval }(setupRetryInterval)
	setupRetryInterval = time.Millisecond
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: HypothesisLoopNamespace}}
	guaranteed := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: PriorityClassGuaranteed}}
	burst := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: PriorityClassBurst}}
	kube := chartKube(t, namespace, guaranteed, burst)
	unreachable := 3
	kube.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if unreachable == 0 {
			return false, nil, nil
		}
		unreachable--
		return true, nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := (&JobWorkloadClient{kube: kube}).SetupCluster(ctx); err != nil || unreachable != 0 {
		t.Fatalf("SetupCluster = %v after %d unreachable attempts left, want success once the API server answers", err, unreachable)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	unreachable = 1 << 30
	if err := (&JobWorkloadClient{kube: kube}).SetupCluster(ctx); !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("SetupCluster against an API server that never answers = %v, want the unreachable error once ctx ends", err)
	}
}
