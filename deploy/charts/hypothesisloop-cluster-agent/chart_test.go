package clusteragent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const secretToken = "hls_dGhlLXNlY3JldA.c2lnbmF0dXJl"

// jobsNamespace is where every job runs and the prefix of its PriorityClasses, which the agent
// names on each pod (k8sexec's chart test holds the agent to them).
const jobsNamespace = "hypothesisloop-jobs"

// minKubeVersion is the oldest Kubernetes the chart installs on (Chart.yaml kubeVersion).
const minKubeVersion = "1.34.0"

func envValue(container any, name string) any {
	for _, env := range container.(map[string]any)["env"].([]any) {
		if env.(map[string]any)["name"] == name {
			return env.(map[string]any)["value"]
		}
	}
	return nil
}

// make echoes a recipe before running it, so the token must not be spelled into one.
func TestTheMakeRecipeDoesNotEchoTheRuntimeToken(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	cmd := exec.Command("make", "-n", "-C", "../../..", "cluster-agent-up", "CLUSTER=edge", "API_URL=http://api", "METRICS_URL=http://metrics")
	cmd.Env = append(os.Environ(), "RUNTIME_TOKEN="+secretToken)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n cluster-agent-up: %v\n%s", err, out)
	}
	if strings.Contains(string(out), secretToken) {
		t.Fatalf("the recipe make would echo carries the token:\n%s", out)
	}
	if !strings.Contains(string(out), "helm upgrade --install") {
		t.Fatalf("make -n printed no install recipe:\n%s", out)
	}
}

// A pod that tolerates every taint tolerates node.kubernetes.io/unreachable, so the scheduler
// binds it to a node whose machine is gone. The cluster has one cluster-agent: bound there it
// never starts and the cluster never connects.
func TestClusterAgentDoesNotTolerateEveryTaint(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := exec.Command("helm", "template", ".", "--show-only", "templates/cluster-agent.yaml", "--kube-version", minKubeVersion,
		"--set-string", "clusterName=edge,apiURL=http://api,metricsURL=http://metrics,runtimeToken=t,images.registry=r,images.tag=t1").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var deployment appsv1.Deployment
		if err := decoder.Decode(&deployment); err == io.EOF {
			t.Fatal("the chart rendered no cluster-agent Deployment")
		} else if err != nil {
			t.Fatal(err)
		}
		if deployment.Kind != "Deployment" {
			continue
		}
		tolerations := deployment.Spec.Template.Spec.Tolerations
		if len(tolerations) == 0 {
			t.Fatal("cluster-agent tolerates nothing, so it has no node on a cluster whose only node is kept free of workloads")
		}
		for _, toleration := range tolerations {
			if toleration.Key == "" {
				t.Errorf("cluster-agent tolerates every taint: %+v", toleration)
			}
		}
		return
	}
}

// render returns every object the chart renders, hooks included, as generic maps.
func render(t *testing.T, set ...string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := []string{"template", "hypothesisloop-cluster-agent", ".", "--namespace", "hypothesisloop", "--kube-version", minKubeVersion,
		"--set-string", "clusterName=edge,apiURL=http://api,metricsURL=http://metrics,runtimeToken=t,images.registry=r,images.tag=t1"}
	// helm applies --set-string after --set, so a string value replaces one of the defaults above
	// only as another --set-string: such a value is given as "string:key=value".
	for _, value := range set {
		if text, ok := strings.CutPrefix(value, "string:"); ok {
			args = append(args, "--set-string", text)
			continue
		}
		args = append(args, "--set", value)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var objects []map[string]any
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err == io.EOF {
			return objects
		} else if err != nil {
			t.Fatal(err)
		}
		if object != nil {
			objects = append(objects, object)
		}
	}
}

func ofKind(objects []map[string]any, kind string) []map[string]any {
	var found []map[string]any
	for _, object := range objects {
		if object["kind"] == kind {
			found = append(found, object)
		}
	}
	return found
}

func metadata(object map[string]any, field string) string {
	value, _ := object["metadata"].(map[string]any)[field].(string)
	return value
}

// A cluster's owner reads what the runtime may do before installing it. It may change things in
// the jobs namespace only, and outside it only read.
func TestTheRuntimeChangesNothingOutsideTheJobsNamespace(t *testing.T) {
	objects := render(t)
	for _, role := range ofKind(objects, "ClusterRole") {
		for _, rule := range role["rules"].([]any) {
			for _, verb := range rule.(map[string]any)["verbs"].([]any) {
				if verb != "get" && verb != "list" && verb != "watch" {
					t.Errorf("ClusterRole %s grants %s on %v across the cluster", metadata(role, "name"), verb, rule.(map[string]any)["resources"])
				}
			}
		}
	}
	roles := ofKind(objects, "Role")
	if len(roles) != 2 {
		t.Fatalf("the chart rendered %d Roles, want one for each agent", len(roles))
	}
	for _, kind := range []string{"Role", "RoleBinding"} {
		for _, object := range ofKind(objects, kind) {
			if metadata(object, "namespace") != jobsNamespace {
				t.Errorf("%s %s is in namespace %q", kind, metadata(object, "name"), metadata(object, "namespace"))
			}
		}
	}
}

// The agent holds no right to create them, so a job would never start on a cluster whose chart
// names them differently from what the agent puts on a pod.
func TestTheChartInstallsTheNamespaceAndPriorityClassesJobsName(t *testing.T) {
	objects := render(t)
	namespaces := ofKind(objects, "Namespace")
	if len(namespaces) != 1 || metadata(namespaces[0], "name") != jobsNamespace {
		t.Fatalf("namespaces rendered: %v", namespaces)
	}
	classes := map[string]map[string]any{}
	for _, class := range ofKind(objects, "PriorityClass") {
		classes[metadata(class, "name")] = class
	}
	guaranteed, burst := classes[jobsNamespace+"-guaranteed"], classes[jobsNamespace+"-burst"]
	if len(classes) != 2 || guaranteed == nil || burst == nil {
		t.Fatalf("PriorityClasses rendered: %v", classes)
	}
	if guaranteed["value"].(float64) <= burst["value"].(float64) {
		t.Errorf("guaranteed = %v, burst = %v: a guaranteed job must come first", guaranteed["value"], burst["value"])
	}
	for name, class := range classes {
		if class["preemptionPolicy"] != "Never" || class["globalDefault"] != false {
			t.Errorf("%s: preemptionPolicy = %v, globalDefault = %v; unasked, a job pod evicts nothing and is nobody's default", name, class["preemptionPolicy"], class["globalDefault"])
		}
	}
	for _, class := range ofKind(render(t, "jobs.preemptLowerPriorityPods=true"), "PriorityClass") {
		if class["preemptionPolicy"] != "PreemptLowerPriority" {
			t.Errorf("%s: preemptionPolicy = %v with preemption asked for", metadata(class, "name"), class["preemptionPolicy"])
		}
	}
}

// A job runs code of any org. It must not reach another org's job, the cluster's own services
// or the cloud's instance metadata, where a node's credentials are served, and no pod may reach it.
func TestJobsReachNoPodNorTheMetadataEndpoint(t *testing.T) {
	isolation := func(set ...string) networkingv1.NetworkPolicy {
		t.Helper()
		policies := ofKind(render(t, set...), "NetworkPolicy")
		if len(policies) != 1 {
			t.Fatalf("the chart rendered %d NetworkPolicies, want the jobs namespace's one", len(policies))
		}
		raw, err := json.Marshal(policies[0])
		if err != nil {
			t.Fatal(err)
		}
		var policy networkingv1.NetworkPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			t.Fatal(err)
		}
		return policy
	}
	policy := isolation()
	if policy.Namespace != jobsNamespace || len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("the policy selects %v in %q, want every pod of %s", policy.Spec.PodSelector, policy.Namespace, jobsNamespace)
	}
	if !slices.Equal(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) || len(policy.Spec.Ingress) != 0 {
		t.Fatalf("policy types %v with ingress %v, want both types and no ingress allowed", policy.Spec.PolicyTypes, policy.Spec.Ingress)
	}
	var blocks []networkingv1.IPBlock
	for _, rule := range policy.Spec.Egress {
		if len(rule.To) == 0 {
			for _, port := range rule.Ports {
				if port.Port == nil || port.Port.IntValue() != 53 {
					t.Errorf("a job reaches every address on port %v, want only DNS's", port.Port)
				}
			}
			continue
		}
		for _, peer := range rule.To {
			if peer.IPBlock == nil || peer.PodSelector != nil || peer.NamespaceSelector != nil {
				t.Errorf("a job reaches %+v, want address blocks only", peer)
				continue
			}
			blocks = append(blocks, *peer.IPBlock)
		}
	}
	want := []networkingv1.IPBlock{{CIDR: "0.0.0.0/0", Except: []string{"169.254.0.0/16", "10.42.0.0/16", "10.43.0.0/16"}}}
	if !reflect.DeepEqual(blocks, want) {
		t.Errorf("a job reaches %+v, want %+v", blocks, want)
	}
	other := isolation("jobs.network.clusterCIDRs={10.0.0.0/14,10.4.0.0/19}")
	if except := other.Spec.Egress[1].To[0].IPBlock.Except; !slices.Equal(except, []string{"169.254.0.0/16", "10.0.0.0/14", "10.4.0.0/19"}) {
		t.Errorf("with the cluster's own CIDRs a job is kept from %v", except)
	}
}

func podSpec(object map[string]any) map[string]any {
	return object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func TestNodeAgentRunsOnlyOnTheNodesItIsLimitedTo(t *testing.T) {
	if selector, limited := podSpec(ofKind(render(t), "DaemonSet")[0])["nodeSelector"]; limited {
		t.Errorf("unasked, node-agent is limited to %v", selector)
	}
	selector, _ := podSpec(ofKind(render(t, "nodeAgent.nodeSelector.pool=jobs"), "DaemonSet")[0])["nodeSelector"].(map[string]any)
	if len(selector) != 1 || selector["pool"] != "jobs" {
		t.Errorf("nodeSelector = %v, want pool=jobs", selector)
	}
}

// Uninstalling only the release would leave the cluster registered, its jobs placed on it and
// their pods running with no agent.
func TestUninstallDetachesTheClusterFirst(t *testing.T) {
	hooks := ofKind(render(t), "Job")
	if len(hooks) != 1 {
		t.Fatalf("the chart rendered %d Jobs, want the detach hook", len(hooks))
	}
	annotations := hooks[0]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["helm.sh/hook"] != "pre-delete" {
		t.Errorf("hook = %v, want pre-delete", annotations["helm.sh/hook"])
	}
	spec := podSpec(hooks[0])
	command := spec["containers"].([]any)[0].(map[string]any)["command"].([]any)
	if len(command) != 2 || command[0] != "/cluster-agent" || command[1] != "detach" {
		t.Errorf("command = %v", command)
	}
	if spec["serviceAccountName"] != "hypothesisloop-cluster-agent" {
		t.Errorf("the hook runs as %v, which may not delete this cluster's jobs", spec["serviceAccountName"])
	}
	if left := ofKind(render(t, "detachOnUninstall=false"), "Job"); len(left) != 0 {
		t.Errorf("with detachOnUninstall=false the chart still rendered %d Jobs", len(left))
	}
}

// helm test runs the check as a hook Pod in the release namespace, reading the token from the
// release's Secret, never from its own spec.
func TestTheCheckIsATestHookPodThatReadsTheTokenFromTheSecret(t *testing.T) {
	pods := ofKind(render(t), "Pod")
	if len(pods) != 1 {
		t.Fatalf("the chart rendered %d Pods, want the check", len(pods))
	}
	pod := pods[0]
	annotations := pod["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["helm.sh/hook"] != "test" || annotations["helm.sh/hook-delete-policy"] != "before-hook-creation,hook-succeeded" {
		t.Errorf("annotations = %v, want a test hook deleted once it passes", annotations)
	}
	if metadata(pod, "namespace") != "hypothesisloop" {
		t.Errorf("namespace = %q, want the release's", metadata(pod, "namespace"))
	}
	spec := pod["spec"].(map[string]any)
	initContainer := spec["initContainers"].([]any)[0].(map[string]any)
	if initContainer["image"] != "r/hypothesisloop-node-agent:t1" {
		t.Errorf("init image = %v, want the node-agent's", initContainer["image"])
	}
	check := spec["containers"].([]any)[0].(map[string]any)
	if check["image"] != "r/hypothesisloop-cluster-agent:t1" || fmt.Sprint(check["command"]) != "[/cluster-agent check]" {
		t.Errorf("check = %v %v, want the cluster-agent's check", check["image"], check["command"])
	}
	for name, want := range map[string]string{"CLUSTER_NAME": "edge", "API_URL": "http://api", "METRICS_URL": "http://metrics"} {
		if got := envValue(check, name); got != want {
			t.Errorf("%s = %v, want %s", name, got, want)
		}
	}
	var token any
	for _, env := range check["env"].([]any) {
		if env.(map[string]any)["name"] == "RUNTIME_TOKEN" {
			token = env.(map[string]any)["valueFrom"]
		}
	}
	if ref, _ := token.(map[string]any)["secretKeyRef"].(map[string]any); ref["name"] != "hypothesisloop-runtime-token" || ref["key"] != "token" {
		t.Errorf("RUNTIME_TOKEN from %v, want the Secret hypothesisloop-runtime-token", token)
	}
}

// The DaemonSet that trusts a registry of plain HTTP writes containerd's hosts.toml for the
// registry's host and port, without its path, on every node; unasked, there is none.
func TestTheInsecureRegistryDaemonSetWritesTheRegistrysHostsFileOnlyWhenEnabled(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	for _, ds := range ofKind(render(t), "DaemonSet") {
		if metadata(ds, "name") == "hypothesisloop-insecure-registry" {
			t.Fatal("unasked, the chart renders the insecure registry DaemonSet")
		}
	}
	var found map[string]any
	for _, ds := range ofKind(render(t, "insecureRegistry.enabled=true", "string:images.registry=100.64.0.5:5000/images"), "DaemonSet") {
		if metadata(ds, "name") == "hypothesisloop-insecure-registry" {
			found = ds
		}
	}
	if found == nil {
		t.Fatal("insecureRegistry.enabled=true renders no DaemonSet")
	}
	spec := podSpec(found)
	script := fmt.Sprint(spec["containers"].([]any)[0].(map[string]any)["command"])
	for _, want := range []string{`dir="/hosts/100.64.0.5:5000"`, `server = "http://%s"`, "skip_verify = true"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "/images") {
		t.Errorf("the hosts directory carries the registry's path:\n%s", script)
	}
	if tolerations := fmt.Sprint(spec["tolerations"]); !strings.Contains(tolerations, "Exists") {
		t.Errorf("tolerations = %s, want every node", tolerations)
	}
	volume := fmt.Sprint(spec["volumes"])
	if !strings.Contains(volume, "/var/lib/rancher/k3s/agent/etc/containerd/certs.d") {
		t.Errorf("volumes = %s, want k3s's hosts directory by default", volume)
	}
}

func TestTheChartRefusesAKubernetesWithoutDRAv1(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	out, err := exec.Command("helm", "template", ".", "--kube-version", "1.33.0",
		"--set-string", "clusterName=edge,apiURL=http://api,metricsURL=http://metrics,runtimeToken=t,images.registry=r,images.tag=t1").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "kubeVersion") {
		t.Errorf("helm template on 1.33: err = %v\n%s", err, out)
	}
}
