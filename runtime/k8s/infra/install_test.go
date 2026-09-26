package infra

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
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
