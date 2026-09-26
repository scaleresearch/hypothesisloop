import type { RegisteredCluster } from '@/types'

export type ClusterKind = 'kubernetes' | 'bare-metal'

// What `hl cluster add --bare-metal` prints (cli/accounts.go bareMetalScript). It runs on the node
// as root: the node's engine copies the agent out of the image that carries it, and the agent
// installs itself as a systemd unit and asks for the runtime token. $1 is the cluster name, $2 the
// API address, $3 the image that carries the agent and $4 the job image.
export const BARE_METAL_SCRIPT =
  'engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; ' +
  'staged="$("$engine" create "$3")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && ' +
  'exec /usr/local/bin/bare-agent.new install --cluster "$1" --api-url "$2" --job-image "$4"'

// What `hl cluster add` prints (cli/accounts.go kubernetesScript). It reads the runtime token,
// without echo when a person types it, and hands it to helm on standard input. $1 is the chart,
// $2 its version, the rest the chart's values.
export const KUBERNETES_SCRIPT =
  '[ -t 0 ] && { printf "Runtime token: " >&2; trap "stty echo" EXIT; stty -echo; }; IFS= read -r token; [ -t 0 ] && echo >&2; ' +
  'chart="$1" version="$2"; shift 2; ' +
  'printf %s "$token" | helm upgrade --install hypothesisloop-cluster-agent "$chart" --version "$version" ' +
  '--namespace hypothesisloop --create-namespace --take-ownership --set-file runtimeToken=/dev/stdin "$@"'

// Neither command holds the runtime token, so it is in no shell history. Each asks for it when it
// runs, and it is shown beside the command, so the machine needs neither hl nor an API key.
export function startCommand(kind: ClusterKind, cluster: RegisteredCluster): string {
  const name = shellQuote(cluster.name)
  if (kind === 'bare-metal') {
    const image = shellQuote(`${cluster.registry}/hypothesisloop-bare-agent:${cluster.image_tag}`)
    return `sudo sh -c ${shellQuote(BARE_METAL_SCRIPT)} sh ${name} ${shellQuote(cluster.api_url)} ${image} ${shellQuote(cluster.job_image)}`
  }
  const set = (key: string, value: string) => ` --set-string ${shellQuote(`${key}=${helmValue(value)}`)}`
  return `sh -c ${shellQuote(KUBERNETES_SCRIPT)} sh ${shellQuote(`oci://${cluster.registry}/charts/hypothesisloop-cluster-agent`)} ${shellQuote(`0.1.0+${cluster.image_tag}`)}` +
    set('clusterName', cluster.name) + set('apiURL', cluster.api_url) + set('metricsURL', cluster.metrics_url) +
    set('images.registry', cluster.registry) + set('images.tag', cluster.image_tag) + set('jobImage', cluster.job_image)
}

// helm splits a --set-string value on commas and reads a backslash as an escape.
function helmValue(text: string): string {
  return text.replace(/\\/g, '\\\\').replace(/,/g, '\\,')
}

function shellQuote(text: string): string {
  return `'${text.replaceAll("'", `'\\''`)}'`
}
