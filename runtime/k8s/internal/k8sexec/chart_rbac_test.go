package k8sexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Every fake API server in this package's tests answers only what the chart grants the
// cluster-agent: a call the code makes that the chart does not allow fails the test that made it,
// as it fails in a cluster with a 403.
const clusterAgentServiceAccount = "hypothesisloop-cluster-agent"

// grant is one rule bound to the cluster-agent, holding in namespace, or in every namespace and
// for cluster-scoped resources when namespace is empty.
type grant struct {
	namespace string
	rule      rbacv1.PolicyRule
}

func (g grant) allows(action k8stesting.Action) bool {
	if g.namespace != "" && action.GetNamespace() != g.namespace {
		return false
	}
	resource := action.GetResource().Resource
	if sub := action.GetSubresource(); sub != "" {
		resource += "/" + sub
	}
	if !slices.Contains(g.rule.APIGroups, action.GetResource().Group) || !slices.Contains(g.rule.Resources, resource) ||
		!slices.Contains(g.rule.Verbs, action.GetVerb()) {
		return false
	}
	if len(g.rule.ResourceNames) == 0 {
		return true
	}
	named, ok := action.(interface{ GetName() string })
	return ok && slices.Contains(g.rule.ResourceNames, named.GetName())
}

var clusterAgentGrants = sync.OnceValues(func() ([]grant, error) {
	out, err := exec.Command("helm", "template", "hypothesisloop-cluster-agent", "../../infra/chart", "--namespace", "hypothesisloop", "--kube-version", "1.34.0",
		"--set-string", "clusterName=edge,apiURL=http://api,metricsURL=http://metrics,runtimeToken=t,images.registry=r,images.tag=t1",
		"--set", "builder.enabled=true").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("helm template: %w\n%s", err, out)
	}
	roles, clusterRoles := map[string][]rbacv1.PolicyRule{}, map[string][]rbacv1.PolicyRule{}
	var bindings []rbacv1.RoleBinding
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		switch object.GetKind() {
		case "Role", "ClusterRole":
			var role rbacv1.Role
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &role); err != nil {
				return nil, err
			}
			if role.Kind == "Role" {
				roles[role.Namespace+"/"+role.Name] = role.Rules
			} else {
				clusterRoles[role.Name] = role.Rules
			}
		case "RoleBinding", "ClusterRoleBinding":
			var binding rbacv1.RoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &binding); err != nil {
				return nil, err
			}
			if slices.ContainsFunc(binding.Subjects, func(s rbacv1.Subject) bool {
				return s.Kind == "ServiceAccount" && s.Name == clusterAgentServiceAccount
			}) {
				bindings = append(bindings, binding)
			}
		}
	}
	var grants []grant
	for _, binding := range bindings {
		rules := clusterRoles[binding.RoleRef.Name]
		if binding.RoleRef.Kind == "Role" {
			rules = roles[binding.Namespace+"/"+binding.RoleRef.Name]
		}
		if rules == nil {
			return nil, fmt.Errorf("%s %s binds %s %s, which the chart does not render", binding.Kind, binding.Name, binding.RoleRef.Kind, binding.RoleRef.Name)
		}
		for _, rule := range rules {
			grants = append(grants, grant{namespace: binding.Namespace, rule: rule})
		}
	}
	return grants, nil
})

// requireChartGrants fails t, once it ends, for every call made to fake that the chart does not
// grant the cluster-agent.
func requireChartGrants(t *testing.T, fake *k8stesting.Fake) {
	t.Helper()
	grants, err := clusterAgentGrants()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, action := range fake.Actions() {
			if !slices.ContainsFunc(grants, func(g grant) bool { return g.allows(action) }) {
				t.Errorf("the chart does not grant the cluster-agent %s", describe(action))
			}
		}
	})
}

func describe(action k8stesting.Action) string {
	resource := action.GetResource()
	text := action.GetVerb() + " " + resource.Resource
	if sub := action.GetSubresource(); sub != "" {
		text += "/" + sub
	}
	if resource.Group != "" {
		text += "." + resource.Group
	}
	if named, ok := action.(interface{ GetName() string }); ok && named.GetName() != "" {
		text += " " + named.GetName()
	}
	if ns := action.GetNamespace(); ns != "" {
		return text + " in " + ns
	}
	return text + " cluster-wide"
}

// chartKube is a fake clientset holding objects that serves only what the chart grants.
func chartKube(t *testing.T, objects ...runtime.Object) *fake.Clientset {
	t.Helper()
	kube := fake.NewClientset(objects...)
	requireChartGrants(t, &kube.Fake)
	return kube
}

// chartDynamic is a fake dynamic client holding objects that serves only what the chart grants.
func chartDynamic(t *testing.T, listKinds map[schema.GroupVersionResource]string, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	requireChartGrants(t, &dyn.Fake)
	return dyn
}

// The checks above are only as good as their reading of the chart.
func TestTheChartGrantsAreReadAsTheClusterEnforcesThem(t *testing.T) {
	grants, err := clusterAgentGrants()
	if err != nil {
		t.Fatal(err)
	}
	allowed := func(action k8stesting.Action) bool {
		return slices.ContainsFunc(grants, func(g grant) bool { return g.allows(action) })
	}
	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	namespaces := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, tc := range []struct {
		action k8stesting.Action
		want   bool
	}{
		{k8stesting.NewGetAction(pods, HypothesisLoopNamespace, "p"), true},
		{k8stesting.NewListAction(pods, schema.GroupVersionKind{}, "", metav1.ListOptions{}), true},
		{k8stesting.NewDeleteAction(pods, "kube-system", "p"), false},
		{k8stesting.NewGetSubresourceAction(pods, HypothesisLoopNamespace, "log", "p"), true},
		{k8stesting.NewGetSubresourceAction(pods, "kube-system", "log", "p"), false},
		{k8stesting.NewRootGetAction(namespaces, "kube-system"), true},
		{k8stesting.NewRootGetAction(namespaces, "default"), false},
		{k8stesting.NewRootDeleteAction(namespaces, "kube-system"), false},
	} {
		if got := allowed(tc.action); got != tc.want {
			t.Errorf("%s allowed = %v, want %v", describe(tc.action), got, tc.want)
		}
	}
}

// Every operation the cluster-agent runs, once each on a distributed DRA job in an environment,
// against fakes that hold it to the chart: a call no other test reaches is still checked here.
func TestEveryCallTheAgentMakesIsOneTheChartGrants(t *testing.T) {
	ctx := context.Background()
	kube := chartKube(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: HypothesisLoopNamespace}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}},
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: PriorityClassGuaranteed}},
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: PriorityClassBurst}},
		totalCapacityTestNode("worker-a", 8, 32), totalCapacityTestNode("worker-b", 8, 32),
	)
	c := &JobWorkloadClient{apiURL: testAPIURL, kube: kube, dyn: chartDynamic(t, map[schema.GroupVersionResource]string{
		resourceSliceGVR: "ResourceSliceList", resourceClaimGVR: "ResourceClaimList",
		deviceClassGVR: "DeviceClassList", resourceClaimTemplateGVR: "ResourceClaimTemplateList",
	}, tenstorrentDeviceClass(),
		tenstorrentResourceSlice("slice-a", "worker-a", "chip-0", "chip-1"),
		tenstorrentResourceSlice("slice-b", "worker-b", "chip-0", "chip-1"))}
	exp := pullTestExperiment("everything", "key-1", environmentImage)
	exp.AcceleratorType, exp.AcceleratorCount = "tenstorrent.com/chipArch=blackhole", 4
	exp.Job.AcceleratorType, exp.Job.AcceleratorCount, exp.Job.NumNodes = exp.AcceleratorType, 2, 2

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("SetupCluster", c.SetupCluster(ctx))
	_, err := c.GetClusterID(ctx)
	must("GetClusterID", err)
	must("CreateWorkload", c.CreateWorkload(ctx, exp, pullLogin("token")))
	must("CreateWorkload again", c.CreateWorkload(ctx, exp, pullLogin("token")))
	must("RefreshPull", c.RefreshPull(ctx, exp, pullLogin("next-token")))
	_, err = c.WorkloadMatchesDesired(ctx, exp)
	must("WorkloadMatchesDesired", err)

	pod := requestingPod("exp-everything-0", "worker-a", true, 1000)
	pod.Namespace, pod.Labels[workloadkeys.ExperimentID] = HypothesisLoopNamespace, exp.ID
	pod.Spec.TerminationGracePeriodSeconds = int64Ptr(5)
	must("add a pod", kube.Tracker().Add(pod))
	_, err = c.PollJobStatus(ctx, exp.ID)
	must("PollJobStatus", err)
	_, _, _, _, _, err = c.PollPhaseDetail(ctx, exp.ID, exp.AttemptCount)
	must("PollPhaseDetail", err)
	_, err = c.FetchLogTail(ctx, exp.ID, 10)
	must("FetchLogTail", err)
	_, _, _, err = c.ResolveAdmittedAcceleratorType(ctx, exp.ID)
	must("ResolveAdmittedAcceleratorType", err)
	_, err = c.ListManagedJobs(ctx)
	must("ListManagedJobs", err)
	_, err = c.ListManagedJobsForStatus(ctx)
	must("ListManagedJobsForStatus", err)
	_, err = c.ListManagedAuxiliaryWorkloads(ctx)
	must("ListManagedAuxiliaryWorkloads", err)
	_, err = c.GetLiveNodeResourceCapacity(ctx)
	must("GetLiveNodeResourceCapacity", err)
	_, err = c.GetNodeTotalCapacity(ctx)
	must("GetNodeTotalCapacity", err)
	_, _, _, _, err = c.GetLiveAcceleratorCapacitySnapshot(ctx)
	must("GetLiveAcceleratorCapacitySnapshot", err)
	must("DeleteWorkload", c.DeleteWorkload(ctx, exp.ID, true))
}
