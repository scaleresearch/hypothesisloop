package accounts

import (
	"fmt"
	"strings"
)

// AttachCommands are the commands that attach a registered cluster, one for each kind of runtime.
// Neither holds the runtime token: each asks for it on standard input.
type AttachCommands struct {
	Kubernetes string `json:"kubernetes" doc:"Run where helm reaches the Kubernetes cluster. Installs the cluster's agent with helm."`
	BareMetal  string `json:"bare_metal" doc:"Run on the node. Installs the node's agent as a systemd unit, which starts jobs as containers with docker or podman."`
}

func attachCommands(registered RegisteredCluster) AttachCommands {
	return AttachCommands{Kubernetes: kubernetesCommand(registered), BareMetal: bareMetalCommand(registered)}
}

// kubernetesScript reads the runtime token, without echo when a person types it, and hands it
// to helm on standard input, so it is in no shell history and no process's arguments. helm
// installs the runtime chart that make images pushed beside the images, and with
// --take-ownership takes over the jobs namespace an install from before the chart created.
// $1 is the chart, $2 its version, the rest the chart's values.
const kubernetesScript = `[ -t 0 ] && { printf "Runtime token: " >&2; trap "stty echo" EXIT; stty -echo; }; IFS= read -r token; [ -t 0 ] && echo >&2; ` +
	`chart="$1" version="$2"; shift 2; ` +
	`printf %s "$token" | helm upgrade --install hypothesisloop-cluster-agent "$chart" --version "$version" ` +
	`--namespace hypothesisloop --create-namespace --take-ownership --set-file runtimeToken=/dev/stdin "$@"`

// kubernetesCommand attaches a Kubernetes cluster and holds no token.
func kubernetesCommand(registered RegisteredCluster) string {
	set := func(key, value string) string {
		return " --set-string " + shellQuote(key+"="+helmValue(value))
	}
	return fmt.Sprintf("sh -c %s sh %s %s", shellQuote(kubernetesScript),
		shellQuote("oci://"+registered.Registry+"/charts/hypothesisloop-cluster-agent"), shellQuote("0.1.0+"+registered.ImageTag)) +
		set("clusterName", registered.Name) + set("apiURL", registered.APIURL) + set("metricsURL", registered.MetricsURL) +
		set("images.registry", registered.Registry) + set("images.tag", registered.ImageTag)
}

// helmValue writes text as one value of helm's --set-string, which splits on commas and reads a
// backslash as an escape.
func helmValue(text string) string {
	return strings.NewReplacer(`\`, `\\`, ",", `\,`).Replace(text)
}

// bareMetalScript runs on the node as root. The node's engine copies the agent out of the image
// that carries it, with no container started, and the agent installs itself: it knows its own
// unit and settings file, and asks for the runtime token on standard input. It is staged
// beside its place because a running agent's binary cannot be written over. $1 is the cluster
// name, $2 the API address and $3 the image that carries the agent.
const bareMetalScript = `engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; ` +
	`staged="$("$engine" create "$3")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && ` +
	`exec /usr/local/bin/bare-agent.new install --cluster "$1" --api-url "$2"`

// bareMetalCommand installs the agent of a bare-metal node and holds no token.
func bareMetalCommand(registered RegisteredCluster) string {
	return fmt.Sprintf("sudo sh -c %s sh %s %s %s", shellQuote(bareMetalScript), shellQuote(registered.Name), shellQuote(registered.APIURL),
		shellQuote(registered.Registry+"/hypothesisloop-bare-agent:"+registered.ImageTag))
}

// shellQuote writes text as one word of a POSIX shell command, whatever it holds.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}
