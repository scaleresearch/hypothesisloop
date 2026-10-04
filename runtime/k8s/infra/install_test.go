package infra

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/scaleresearch/hypothesisloop/runtime/k8s/internal/k8sexec"
)

const secretToken = "hls_dGhlLXNlY3JldA.c2lnbmF0dXJl"

// fakeBin puts a helm that records its arguments and its stdin, and a kubectl that names a
// context, ahead of the real ones.
func fakeBin(t *testing.T) (dir, argsFile, stdinFile string) {
	t.Helper()
	dir = t.TempDir()
	argsFile, stdinFile = filepath.Join(dir, "helm.args"), filepath.Join(dir, "helm.stdin")
	helm := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat > " + stdinFile + "\n"
	kubectl := "#!/bin/sh\necho test-context\n"
	for name, script := range map[string]string{"helm": helm, "kubectl": kubectl} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, argsFile, stdinFile
}

// A process's arguments are readable by every user of the machine, and helm's are logged by CI.
func TestInstallHandsHelmTheRuntimeTokenOnStdinAndNeverAsAnArgument(t *testing.T) {
	bin, argsFile, stdinFile := fakeBin(t)
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
		"CLUSTER_NAME=edge", "API_URL=http://api", "METRICS_URL=http://metrics", "TAG=t1", "RUNTIME_TOKEN="+secretToken)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	args, _ := os.ReadFile(argsFile)
	stdin, _ := os.ReadFile(stdinFile)
	if strings.Contains(string(args), secretToken) || strings.Contains(string(out), secretToken) {
		t.Errorf("the token is in helm's arguments or the script's output:\nargs: %s\noutput: %s", args, out)
	}
	if !strings.Contains(string(args), "--values\n-\n") {
		t.Errorf("helm arguments = %q, want values read from stdin", args)
	}
	if want := `runtimeToken: "` + secretToken + `"`; strings.TrimSpace(string(stdin)) != want {
		t.Errorf("helm stdin = %q, want %q", stdin, want)
	}
	if !strings.Contains(string(args), "clusterName=edge") {
		t.Errorf("helm arguments = %q, want the other values still set", args)
	}
	if !strings.Contains(string(args), "builder.enabled=false\n") {
		t.Errorf("helm arguments = %q, want no builder unasked", args)
	}
	// A cluster that ran the runtime before the chart owned the jobs namespace has one no release
	// owns, and helm refuses to install over it unless told to take it.
	if !strings.Contains(string(args), "--take-ownership\n") {
		t.Errorf("helm arguments = %q, want an older install's jobs namespace taken over", args)
	}
}

func TestInstallWithBuilderTrueEnablesTheBuilder(t *testing.T) {
	bin, argsFile, _ := fakeBin(t)
	for builder, want := range map[string]string{"true": "builder.enabled=true\n", "yes": ""} {
		cmd := exec.Command("bash", "install.sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "BUILDER="+builder,
			"CLUSTER_NAME=edge", "API_URL=http://api", "METRICS_URL=http://metrics", "TAG=t1", "RUNTIME_TOKEN="+secretToken)
		out, err := cmd.CombinedOutput()
		if want == "" {
			if err == nil {
				t.Errorf("BUILDER=%s installed, want it refused", builder)
			}
			continue
		}
		if err != nil {
			t.Fatalf("install.sh: %v\n%s", err, out)
		}
		if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), want) {
			t.Errorf("helm arguments = %q, want %q", args, want)
		}
	}
}

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

func TestInstallLoadsBuildsIntoTheNodeOnlyWithABuilder(t *testing.T) {
	bin, argsFile, _ := fakeBin(t)
	for builder, ok := range map[string]bool{"true": true, "false": false} {
		cmd := exec.Command("bash", "install.sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "BUILDER="+builder, "BUILDER_NODE_IMAGES=/run/k3s/containerd/containerd.sock",
			"CLUSTER_NAME=edge", "API_URL=http://api", "METRICS_URL=http://metrics", "TAG=t1", "RUNTIME_TOKEN="+secretToken)
		out, err := cmd.CombinedOutput()
		if !ok {
			if err == nil {
				t.Errorf("BUILDER=%s with BUILDER_NODE_IMAGES installed, want it refused", builder)
			}
			continue
		}
		if err != nil {
			t.Fatalf("install.sh: %v\n%s", err, out)
		}
		if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "builder.nodeImages.containerdSocket=/run/k3s/containerd/containerd.sock\n") {
			t.Errorf("helm arguments = %q, want the socket", args)
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
	if !strings.Contains(string(out), "runtime/k8s/infra/install.sh") {
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
	out, err := exec.Command("helm", "template", "chart", "--show-only", "templates/cluster-agent.yaml",
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
	args := []string{"template", "hypothesisloop-cluster-agent", "chart", "--namespace", "hypothesisloop",
		"--set-string", "clusterName=edge,apiURL=http://api,metricsURL=http://metrics,runtimeToken=t,images.registry=r,images.tag=t1"}
	for _, value := range set {
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
			if metadata(object, "namespace") != k8sexec.HypothesisLoopNamespace {
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
	if len(namespaces) != 1 || metadata(namespaces[0], "name") != k8sexec.HypothesisLoopNamespace {
		t.Fatalf("namespaces rendered: %v", namespaces)
	}
	classes := map[string]map[string]any{}
	for _, class := range ofKind(objects, "PriorityClass") {
		classes[metadata(class, "name")] = class
	}
	guaranteed, burst := classes[k8sexec.PriorityClassGuaranteed], classes[k8sexec.PriorityClassBurst]
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
