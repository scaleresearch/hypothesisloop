package accounts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	clusteragent "github.com/scaleresearch/hypothesisloop/deploy/charts/hypothesisloop-cluster-agent"
)

// The command is run as printed: a shell splits it into helm's own arguments, and the token piped
// to it reaches helm on stdin and nowhere else.
func TestTheKubernetesCommandRunsHelmWithTheTokenOnStdin(t *testing.T) {
	dir := t.TempDir()
	argsFile, stdinFile := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	helm := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat > " + stdinFile + "\n"
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(helm), 0o755); err != nil {
		t.Fatal(err)
	}
	registered := RegisteredCluster{ClusterView: ClusterView{Name: "gpu-east"}, APIURL: "https://api.example/a?ids=1,2", MetricsURL: "https://metrics.example/it's",
		Registry: "registry.example/images", ImageTag: "abc1234"}
	const token = "hls_dGhlLXNlY3JldA.c2lnbmF0dXJl"
	cmd := exec.Command("sh", "-c", kubernetesCommand(registered))
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(token + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the command: %v\n%s", err, out)
	}
	args, _ := os.ReadFile(argsFile)
	want := strings.Join([]string{"upgrade", "--install", "hypothesisloop-cluster-agent", "oci://registry.example/images/charts/hypothesisloop-cluster-agent",
		"--version", clusteragent.Version("abc1234"), "--namespace", "hypothesisloop", "--create-namespace", "--take-ownership", "--set-file", "runtimeToken=/dev/stdin",
		"--set-string", "clusterName=gpu-east", "--set-string", `apiURL=https://api.example/a?ids=1\,2`, "--set-string", "metricsURL=https://metrics.example/it's",
		"--set-string", "images.registry=registry.example/images", "--set-string", "images.tag=abc1234"}, "\n") + "\n"
	if string(args) != want {
		t.Errorf("helm arguments:\n%s\nwant:\n%s", args, want)
	}
	if stdin, _ := os.ReadFile(stdinFile); string(stdin) != token {
		t.Errorf("helm stdin = %q, want the token", stdin)
	}
}

// A registry of plain HTTP, as the local dev registry is, is read so by helm and by every node:
// over HTTPS neither the chart nor an image is found.
func TestTheKubernetesCommandReadsARegistryOfPlainHTTPSo(t *testing.T) {
	registered := RegisteredCluster{ClusterView: ClusterView{Name: "tt-box"}, APIURL: "http://100.64.0.5:8081", MetricsURL: "http://100.64.0.5:8084",
		Registry: "100.64.0.5:5000", ImageTag: "abc1234"}
	if command := kubernetesCommand(registered); strings.Contains(command, "--plain-http") || strings.Contains(command, "insecureRegistry") {
		t.Errorf("command for a registry of TLS:\n%s\nwant neither --plain-http nor insecureRegistry", command)
	}
	registered.RegistryPlainHTTP = true
	command := kubernetesCommand(registered)
	for _, want := range []string{"'oci://100.64.0.5:5000/charts/hypothesisloop-cluster-agent' --plain-http", "--set insecureRegistry.enabled=true"} {
		if !strings.Contains(command, want) {
			t.Errorf("command for a registry of plain HTTP:\n%s\nwant %q", command, want)
		}
	}
}

// The command copies the agent out of the image with the node's engine and runs it with the
// arguments printed after the image, which a dry run extends with --dry-run.
func TestTheBareMetalCommandRunsTheAgentFromItsImage(t *testing.T) {
	dir := t.TempDir()
	argsFile, staged := filepath.Join(dir, "args"), filepath.Join(dir, "bare-agent.new")
	scripts := map[string]string{
		// sudo runs the command with the agent staged in dir, where a test may write.
		"sudo": "#!/bin/sh\nscript=$(printf %s \"$3\" | sed 's|/usr/local/bin/bare-agent.new|" + staged + "|g')\nshift 3\nexec sh -c \"$script\" \"$@\"\n",
		"docker": "#!/bin/sh\ncase \"$1\" in\n  create) [ \"$2\" = registry.example/images/hypothesisloop-bare-agent:abc1234 ] && echo container-1 ;;\n" +
			"  cp) [ \"$2\" = container-1:/usr/local/bin/bare-agent ] && printf '#!/bin/sh\\nprintf \"%%s\\\\n\" \"$@\" > " + argsFile + "\\n' > \"$3\" && chmod +x \"$3\" ;;\nesac\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registered := RegisteredCluster{ClusterView: ClusterView{Name: "gpu-east"}, APIURL: "https://api.example/a?b=$(id)", Registry: "registry.example/images", ImageTag: "abc1234"}
	cmd := exec.Command("sh", "-c", bareMetalCommand(registered)+" --dry-run")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the command: %v\n%s", err, out)
	}
	args, _ := os.ReadFile(argsFile)
	if want := "install\n--cluster\ngpu-east\n--api-url\nhttps://api.example/a?b=$(id)\n--dry-run\n"; string(args) != want {
		t.Errorf("bare-agent arguments = %q, want %q", args, want)
	}
}
