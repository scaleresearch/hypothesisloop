package k8sexec

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
		kube := fake.NewClientset(tc.installed...)
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
