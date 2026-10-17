package clusteragent

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const secretToken = "hls_dGhlLXNlY3JldA.c2lnbmF0dXJl"

// jobsNamespace is where every job runs and the prefix of its PriorityClasses, which the agent
// names on each pod (k8sexec's chart test holds the agent to them).
const jobsNamespace = "hypothesisloop-jobs"

// minKubeVersion is the oldest Kubernetes the chart installs on (Chart.yaml kubeVersion).
const minKubeVersion = "1.34.0"

// The builder is a rootless sidecar of the cluster-agent reached over a socket in the pod: no
// port, no certificate, nothing privileged, and its cache on a volume of its own.
func TestTheBuilderIsARootlessSidecarOfTheAgentOnlyWhenEnabled(t *testing.T) {
	plain := render(t)
	if services := ofKind(plain, "Service"); len(services) != 0 {
		t.Fatalf("unasked, the chart rendered Services %v", services)
	}
	if claims := ofKind(plain, "PersistentVolumeClaim"); len(claims) != 0 {
		t.Fatalf("unasked, the chart rendered claims %v", claims)
	}
	for _, deployment := range ofKind(plain, "Deployment") {
		spec := podSpec(deployment)
		if _, sidecar := spec["initContainers"]; sidecar || envValue(spec["containers"].([]any)[0], "HYPOTHESISLOOP_BUILDS") != nil {
			t.Errorf("unasked, %s builds", metadata(deployment, "name"))
		}
	}

	objects := render(t, "builder.enabled=true")
	if services := ofKind(objects, "Service"); len(services) != 0 {
		t.Errorf("the builder is reached through Services %v", services)
	}
	if secrets := ofKind(objects, "Secret"); len(secrets) != 1 || metadata(secrets[0], "name") != "hypothesisloop-runtime-token" {
		t.Errorf("the chart rendered Secrets %v, want the runtime token's alone", secrets)
	}
	if claims := ofKind(objects, "PersistentVolumeClaim"); len(claims) != 1 {
		t.Errorf("the chart rendered %d claims, want the builder's cache", len(claims))
	}
	deployments := ofKind(objects, "Deployment")
	if len(deployments) != 1 || metadata(deployments[0], "name") != "hypothesisloop-cluster-agent" {
		t.Fatalf("the chart rendered Deployments %v, want the cluster-agent's alone", deployments)
	}
	spec := podSpec(deployments[0])
	sidecars := spec["initContainers"].([]any)
	if len(sidecars) != 1 {
		t.Fatalf("init containers %v, want the builder", sidecars)
	}
	buildkitd := sidecars[0].(map[string]any)
	if buildkitd["restartPolicy"] != "Always" || !strings.Contains(buildkitd["image"].(string), "-rootless@sha256:") {
		t.Errorf("buildkitd %v, want a rootless sidecar", buildkitd)
	}
	if _, ports := buildkitd["ports"]; ports {
		t.Errorf("buildkitd listens on ports %v", buildkitd["ports"])
	}
	security := buildkitd["securityContext"].(map[string]any)
	if security["privileged"] == true || security["runAsUser"] != float64(1000) {
		t.Errorf("buildkitd security %v, want unprivileged as the rootless user", security)
	}
	if args := buildkitd["args"].([]any); !slices.Contains(args, any("--addr=unix:///run/buildkit/buildkitd.sock")) || !slices.Contains(args, any("--oci-worker-gc-keepstorage=10000,20000,80000")) || len(args) != 3 {
		t.Errorf("buildkitd args %v, want the pod's socket alone and its cache pruned past builder.cache", args)
	}
	// Requested, the builder's share is left out of the capacity the cluster reports.
	requests, _ := buildkitd["resources"].(map[string]any)["requests"].(map[string]any)
	if requests["cpu"] != "4" || requests["memory"] != "8Gi" {
		t.Errorf("buildkitd requests = %v, want builder.requests", requests)
	}
	// Bounded, a RUN that grows past the limit is killed, not the node's jobs.
	if limits, _ := buildkitd["resources"].(map[string]any)["limits"].(map[string]any); limits["memory"] != "8Gi" {
		t.Errorf("buildkitd limits = %v, want builder.limits", limits)
	}
	if builds := envValue(spec["containers"].([]any)[0], "HYPOTHESISLOOP_BUILDS"); builds != "unix:///run/buildkit/buildkitd.sock" {
		t.Errorf("HYPOTHESISLOOP_BUILDS = %v, want the sidecar's socket", builds)
	}
	strategy := deployments[0]["spec"].(map[string]any)["strategy"].(map[string]any)
	rolling, _ := strategy["rollingUpdate"].(map[string]any)
	if strategy["type"] != "RollingUpdate" || fmt.Sprint(rolling["maxSurge"]) != "0" || fmt.Sprint(rolling["maxUnavailable"]) != "1" {
		t.Errorf("strategy = %v, two daemons would share the cache during a rollout", strategy)
	}
}

// The node's containerd socket is root on that node, so the agent holds it only when asked, and
// only beside a builder, whose builds are what it loads.
func TestTheAgentHoldsTheNodesContainerdOnlyWhenAskedToLoadBuilds(t *testing.T) {
	const socket = "/run/k3s/containerd/containerd.sock"
	for name, set := range map[string][]string{
		"no builder":                   nil,
		"a builder that loads nothing": {"builder.enabled=true"},
		"loading without a builder":    {"builder.nodeImages.containerdSocket=" + socket},
	} {
		spec := podSpec(ofKind(render(t, set...), "Deployment")[0])
		volumes, _ := spec["volumes"].([]any)
		for _, volume := range volumes {
			if _, host := volume.(map[string]any)["hostPath"]; host {
				t.Errorf("%s: the agent mounts %v from its node", name, volume)
			}
		}
		if loads := envValue(spec["containers"].([]any)[0], "HYPOTHESISLOOP_NODE_IMAGES"); loads != nil {
			t.Errorf("%s: HYPOTHESISLOOP_NODE_IMAGES = %v", name, loads)
		}
	}

	spec := podSpec(ofKind(render(t, "builder.enabled=true", "builder.nodeImages.containerdSocket="+socket), "Deployment")[0])
	var hostPath map[string]any
	for _, volume := range spec["volumes"].([]any) {
		if volume.(map[string]any)["name"] == "node-containerd" {
			hostPath = volume.(map[string]any)["hostPath"].(map[string]any)
		}
	}
	if hostPath["path"] != socket || hostPath["type"] != "Socket" {
		t.Fatalf("node-containerd %v, want the socket asked for", hostPath)
	}
	agent := spec["containers"].([]any)[0].(map[string]any)
	mounted := false
	for _, mount := range agent["volumeMounts"].([]any) {
		mount := mount.(map[string]any)
		mounted = mounted || mount["name"] == "node-containerd" && mount["mountPath"] == "/run/node-containerd/containerd.sock"
	}
	if loads := envValue(agent, "HYPOTHESISLOOP_NODE_IMAGES"); !mounted || loads != "/run/node-containerd/containerd.sock" {
		t.Errorf("mounted %v, HYPOTHESISLOOP_NODE_IMAGES = %v, want the mounted socket", mounted, loads)
	}
	// The node is named, so a build loaded there places the jobs waiting for it on it.
	var node any
	for _, env := range agent["env"].([]any) {
		if env.(map[string]any)["name"] == "HYPOTHESISLOOP_NODE_NAME" {
			node = env.(map[string]any)["valueFrom"]
		}
	}
	if field, _ := node.(map[string]any)["fieldRef"].(map[string]any); field["fieldPath"] != "spec.nodeName" {
		t.Errorf("HYPOTHESISLOOP_NODE_NAME from %v, want the pod's node", node)
	}
}

func TestTheAgentHasTheMemoryALoadNeedsOnlyWhenItLoads(t *testing.T) {
	const socket = "/run/k3s/containerd/containerd.sock"
	for name, want := range map[string]struct {
		set              []string
		memory, memLimit string
	}{
		"no builder":                   {nil, "64Mi", ""},
		"a builder that loads nothing": {[]string{"builder.enabled=true"}, "128Mi", ""},
		"a builder that loads":         {[]string{"builder.enabled=true", "builder.nodeImages.containerdSocket=" + socket}, "1Gi", "900MiB"},
	} {
		agent := podSpec(ofKind(render(t, want.set...), "Deployment")[0])["containers"].([]any)[0]
		limits := agent.(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)
		if limits["memory"] != want.memory {
			t.Errorf("%s: memory limit %v, want %s", name, limits["memory"], want.memory)
		}
		if got, _ := envValue(agent, "GOMEMLIMIT").(string); got != want.memLimit {
			t.Errorf("%s: GOMEMLIMIT %q, want %q", name, got, want.memLimit)
		}
	}
}

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
