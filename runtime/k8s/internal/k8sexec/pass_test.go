package k8sexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentloop"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

var _ agentexec.PassScoped = (*JobWorkloadClient)(nil)

// endOfPass tells when a reconcile pass reaches its last step.
type endOfPass struct {
	*JobWorkloadClient
	ended chan struct{}
}

func (e endOfPass) ForPass(ctx context.Context) (agentexec.Executor, error) {
	pass, err := e.JobWorkloadClient.ForPass(ctx)
	if err != nil {
		return nil, err
	}
	return endOfPass{pass.(*JobWorkloadClient), e.ended}, nil
}

func (e endOfPass) ReapTerminal(ctx context.Context, desired map[string]*clusterapi.DesiredJob) error {
	defer close(e.ended)
	return e.JobWorkloadClient.ReapTerminal(ctx, desired)
}

func listsBy(fakes ...*k8stesting.Fake) map[string]int {
	lists := map[string]int{}
	for _, f := range fakes {
		for _, action := range f.Actions() {
			if action.GetVerb() == "list" {
				lists[action.GetResource().Resource]++
			}
		}
	}
	return lists
}

func passTestPod(name, experimentID, node string) *corev1.Pod {
	pod := requestingPod(name, node, true, 1000)
	pod.Namespace, pod.Labels[workloadkeys.ExperimentID] = HypothesisLoopNamespace, experimentID
	pod.Annotations = map[string]string{GraceSecondsAnnotation: "5"}
	return pod
}

// A reconcile pass and a status push each list the cluster's nodes and pods once, however many
// jobs they read about. The one further pod list is each delete's own (signalPods).
func TestAPassListsTheClustersNodesAndPodsOnce(t *testing.T) {
	ctx := context.Background()
	kube := chartKube(t,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}},
		totalCapacityTestNode("worker-a", 8, 32), totalCapacityTestNode("worker-b", 8, 32),
		requestingPod("cni-agent", "worker-a", false, 100),
	)
	dyn := chartDynamic(t, map[schema.GroupVersionResource]string{
		resourceSliceGVR: "ResourceSliceList", resourceClaimGVR: "ResourceClaimList",
		deviceClassGVR: "DeviceClassList", resourceClaimTemplateGVR: "ResourceClaimTemplateList",
	})
	c := &JobWorkloadClient{apiURL: testAPIURL, kube: kube, dyn: dyn}
	running, gone, created := pullTestExperiment("running", "", "busybox"), pullTestExperiment("gone", "", "busybox"), pullTestExperiment("new", "", "busybox")
	for _, exp := range []*clusterapi.DesiredJob{running, gone} {
		if err := c.CreateWorkload(ctx, exp, nil); err != nil {
			t.Fatal(err)
		}
		if err := kube.Tracker().Add(passTestPod("exp-"+exp.ID+"-pod", exp.ID, "worker-a")); err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reconcile") {
			_ = json.NewEncoder(w).Encode(clusterapi.ReconcileAnswer{Jobs: []*clusterapi.DesiredJob{running, created}})
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	ended := make(chan struct{})
	agent := &agentloop.Agent{
		ClusterName: "edge", APIURL: server.URL, HTTPClient: server.Client(),
		Executor:          endOfPass{c, ended},
		ReconcileInterval: time.Hour, StatusInterval: time.Hour,
		Log: func(string, ...any) {},
	}

	kube.ClearActions()
	dyn.ClearActions()
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- agent.Run(runCtx) }()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconcile pass did not end")
	}
	stop()
	<-done
	reconcile := listsBy(&kube.Fake, &dyn.Fake)
	t.Logf("lists in one reconcile pass (3 jobs: one kept, one created, one deleted after its final capture): %v", reconcile)
	if reconcile["nodes"] != 1 || reconcile["pods"] != 2 {
		t.Errorf("one reconcile pass listed nodes %d and pods %d times, want once and twice (the delete's own)", reconcile["nodes"], reconcile["pods"])
	}

	kube.ClearActions()
	dyn.ClearActions()
	pass, err := c.ForPass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pass.ListManagedJobsForStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		observed, err := pass.PollJobStatus(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := pass.ResolveAdmittedAcceleratorType(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pass.FetchLogTail(ctx, id, agentexec.LogTailLines); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, _, err := pass.PollPhaseDetail(ctx, id, observed.Attempt); err != nil {
			t.Fatal(err)
		}
	}
	status := listsBy(&kube.Fake, &dyn.Fake)
	t.Logf("lists in one status push of %d jobs: %v", len(ids), status)
	if status["nodes"] != 1 || status["pods"] != 1 {
		t.Errorf("one status push listed nodes %d and pods %d times, want once each", status["nodes"], status["pods"])
	}
}
