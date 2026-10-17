package accounts

import (
	"fmt"
	"strings"

	clusteragent "github.com/scaleresearch/hypothesisloop/deploy/charts/hypothesisloop-cluster-agent"
)

// AttachCommands are the commands that attach a registered cluster, one for each kind of runtime.
// Neither holds the runtime token: each asks for it on standard input.
type AttachCommands struct {
	Kubernetes string `json:"kubernetes" doc:"Run where helm reaches the Kubernetes cluster. Installs the cluster's agent with helm into the namespace hypothesisloop."`
	BareMetal  string `json:"bare_metal" doc:"Run on the node. Installs the node's agent as a systemd unit, which starts jobs as containers with docker or podman."`
}

// AttachCommandsFor are the commands that attach registered.
func AttachCommandsFor(registered RegisteredCluster) AttachCommands {
	return AttachCommands{Kubernetes: kubernetesCommand(registered), BareMetal: bareMetalCommand(registered)}
}

// askToken reads the runtime token, without echo when a person types it, and hands it to the
// command that follows on standard input, so it is in no shell history and no process's
// arguments.
const askToken = `sh -c '[ -t 0 ] && { printf "Runtime token: " >&2; trap "stty echo" EXIT; stty -echo; }; IFS= read -r token; [ -t 0 ] && echo >&2; printf %s "$token" | "$@"' sh`

// lines joins the words of one shell command, one group of them per line.
func lines(groups ...string) string {
	return strings.Join(groups, " \\\n  ")
}

// kubernetesCommand installs or upgrades the runtime chart that make publish pushed beside the
// images of the release's tag. --take-ownership takes over the jobs namespace an install from before the chart
// created. The values set are the cluster's one config file; the token comes on stdin. A
// registry of plain HTTP is read so by helm, and by every node through the chart's
// insecureRegistry.
func kubernetesCommand(registered RegisteredCluster) string {
	set := func(key, value string) string {
		return "--set-string " + shellQuote(key+"="+helmValue(value))
	}
	chart := "helm upgrade --install hypothesisloop-cluster-agent " + shellQuote("oci://"+registered.Registry+"/charts/hypothesisloop-cluster-agent")
	if registered.RegistryPlainHTTP {
		chart += " --plain-http"
	}
	groups := []string{
		askToken,
		chart,
		"--version " + shellQuote(clusteragent.Version(registered.ImageTag)),
		"--namespace hypothesisloop --create-namespace --take-ownership",
		"--set-file runtimeToken=/dev/stdin",
		set("clusterName", registered.Name),
		set("apiURL", registered.APIURL),
		set("metricsURL", registered.MetricsURL),
		set("images.registry", registered.Registry),
		set("images.tag", registered.ImageTag),
	}
	if registered.RegistryPlainHTTP {
		groups = append(groups, "--set insecureRegistry.enabled=true")
	}
	return lines(groups...)
}

// helmValue writes text as one value of helm's --set-string, which splits on commas and reads a
// backslash as an escape.
func helmValue(text string) string {
	return strings.NewReplacer(`\`, `\\`, ",", `\,`).Replace(text)
}

// fetchBareAgent runs on the node as root. The node's engine copies the agent out of the image
// that carries it, with no container started, and runs it with the arguments that follow; the
// agent installs itself: it knows its own unit and settings file, and asks for the runtime token
// on standard input. It is staged beside its place because a running agent's binary cannot be
// written over. $1 is the image that carries the agent.
const fetchBareAgent = `engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; ` +
	`staged="$("$engine" create "$1")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && ` +
	`shift && exec /usr/local/bin/bare-agent.new "$@"`

// bareMetalCommand installs the agent of a bare-metal node and holds no token.
func bareMetalCommand(registered RegisteredCluster) string {
	return lines(
		fmt.Sprintf("sudo sh -c %s sh", shellQuote(fetchBareAgent)),
		shellQuote(registered.Registry+"/hypothesisloop-bare-agent:"+registered.ImageTag),
		"install --cluster "+shellQuote(registered.Name)+" --api-url "+shellQuote(registered.APIURL),
	)
}

// shellQuote writes text as one word of a POSIX shell command, whatever it holds.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}
