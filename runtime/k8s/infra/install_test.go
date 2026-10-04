package infra

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
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
// context, reaches its API at box.example and gives the builder's Service node port 31234, ahead
// of the real ones.
func fakeBin(t *testing.T) (dir, argsFile, stdinFile string) {
	t.Helper()
	dir = t.TempDir()
	argsFile, stdinFile = filepath.Join(dir, "helm.args"), filepath.Join(dir, "helm.stdin")
	helm := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat > " + stdinFile + "\n"
	kubectl := `#!/bin/sh
case "$*" in
  *"config view"*) echo https://box.example:6443 ;;
  *"get service"*) echo 31234 ;;
  *) echo test-context ;;
esac
`
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
	if strings.Contains(string(args), "builder.") {
		t.Errorf("helm arguments = %q, want no builder unasked", args)
	}
	// A cluster that ran the runtime before the chart owned the jobs namespace has one no release
	// owns, and helm refuses to install over it unless told to take it.
	if !strings.Contains(string(args), "--take-ownership\n") {
		t.Errorf("helm arguments = %q, want an older install's jobs namespace taken over", args)
	}
}

// The control plane reaches the builder at the host the attach reached the cluster's API at, and
// only with the client certificate the attach left it.
func TestInstallWithABuilderLeavesTheControlPlaneItsAddressAndCredentials(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not installed")
	}
	bin, argsFile, _ := fakeBin(t)
	client := filepath.Join(t.TempDir(), "builder")
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "BUILDER_CLIENT_DIR="+client,
		"CLUSTER_NAME=edge", "API_URL=http://api", "METRICS_URL=http://metrics", "TAG=t1", "RUNTIME_TOKEN="+secretToken)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "builder.enabled=true\n") || !strings.Contains(string(args), "builder.tls.key=") {
		t.Errorf("helm arguments = %q, want the builder enabled with its certificate", args)
	}
	if addr, _ := os.ReadFile(filepath.Join(client, "addr")); string(addr) != "tcp://box.example:31234\n" {
		t.Errorf("addr = %q", addr)
	}
	if info, err := os.Stat(filepath.Join(client, "key.pem")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key.pem: %v, %v; want it readable by its owner only", info, err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(client, "cert.pem"), filepath.Join(client, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(client, "ca.pem"))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem holds no certificate")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("the client certificate is not one the builder accepts: %v", err)
	}
}

func TestTheBuilderIsReachedOnANodePortOnlyWhenEnabled(t *testing.T) {
	if services := ofKind(render(t), "Service"); len(services) != 0 {
		t.Fatalf("unasked, the chart rendered Services %v", services)
	}
	objects := render(t, "builder.enabled=true", "builder.tls.ca=c", "builder.tls.cert=c", "builder.tls.key=k")
	services := ofKind(objects, "Service")
	if len(services) != 1 {
		t.Fatalf("the chart rendered %d Services, want the builder's", len(services))
	}
	spec := services[0]["spec"].(map[string]any)
	if spec["type"] != "NodePort" || spec["ports"].([]any)[0].(map[string]any)["nodePort"] != float64(31234) {
		t.Errorf("builder Service spec = %v", spec)
	}
	for _, deployment := range ofKind(objects, "Deployment") {
		if metadata(deployment, "name") != "hypothesisloop-builder" {
			continue
		}
		args := podSpec(deployment)["containers"].([]any)[0].(map[string]any)["args"].([]any)
		if !slices.Contains(args, any("--tlscacert=/etc/buildkit/tls/ca.pem")) {
			t.Errorf("buildkitd args = %v, want client certificates required", args)
		}
		return
	}
	t.Fatal("the chart rendered no builder Deployment")
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
