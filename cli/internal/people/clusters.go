package people

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

// registeredCluster is what registering a cluster answers.
type registeredCluster struct {
	Name         string `json:"name"`
	RuntimeToken string `json:"runtime_token"`
	APIURL       string `json:"api_url"`
	MetricsURL   string `json:"metrics_url"`
	Registry     string `json:"registry"`
	Attach       struct {
		Kubernetes string `json:"kubernetes"`
		BareMetal  string `json:"bare_metal"`
	} `json:"attach_commands"`
}

func ClusterAdd(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster add", `usage: hl cluster add NAME [--bare-metal] [--org ORG]

Registers the cluster name for the org and prints the one command that attaches it, complete:
the addresses its agent reaches the platform at and the registry and tag of its images. A
cluster attached this way runs the image each job names. Below the command it prints the
cluster's runtime token, which the command asks for when it runs, so the token is in no shell
history and the machine needs neither hl nor an API key. Adding a name the org already has
prints the same again. A cluster takes its own org's jobs only.

Without --bare-metal the command is for a Kubernetes cluster (1.34 or newer): run it with helm
3.17 or newer where kubectl reaches the cluster (add --kube-context CONTEXT to name another,
and --set-string kubeContext=CONTEXT for the notes it prints to name it too). It installs the
runtime's chart from the registry into the namespace hypothesisloop, or upgrades it, and adds
the namespace hypothesisloop-jobs where jobs run. To preview, add helm's own flag
--dry-run=server at the end: the API server checks every object and nothing is installed. It
does not check that the cluster reaches the platform or pulls the images; after the install,
helm test hypothesisloop-cluster-agent -n hypothesisloop --logs does. Pods resolve the addresses with the cluster's
DNS, so a platform on a tailnet is named by its IP. A registry of plain HTTP, as a local
stack's is, makes the command pull the chart over HTTP and set insecureRegistry, which has
every node's containerd pull from it over HTTP. helm uninstall hypothesisloop-cluster-agent -n
hypothesisloop detaches the cluster again: it removes the cluster here, where its jobs go back
to the queue, and stops what it runs there.

Each address the command names that only the platform's own machine reaches is warned about
on stderr.

With --bare-metal it is for one node with docker or podman, which is a cluster of its own. Run
the command on the node. It copies the agent out of its image and installs it as a systemd
unit that starts at boot. It needs root, outbound access to the platform and the image
registries, and nothing inbound. Add --dry-run at the end to check the node and print what it
would write, changing nothing. Running the command of a newer image tag upgrades the agent.
To detach the node again, on the node:

  sudo bare-agent detach        removes the cluster here, stops its jobs and the agent
  sudo bare-agent uninstall     removes the agent from the node

Either kind of cluster offers its cores to jobs that need no accelerator, beside whatever
accelerators it has, with nothing to configure.

hl cluster remove NAME detaches from this side: the agent stops its jobs and stays stopped.`, stderr)
	bareMetal := fs.Bool("bare-metal", false, "print the command for a bare-metal node")
	org := fs.String("org", "", orgFlagHelp)
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	var registered registeredCluster
	if err := c.Do("POST", orgPath(id, "/clusters"), map[string]string{"name": name}, &registered); err != nil {
		return command.Fail(stderr, err)
	}
	attach := registered.Attach.Kubernetes
	if *bareMetal {
		attach = registered.Attach.BareMetal
	}
	fmt.Fprintf(stdout, "%s\n\nIt asks for the runtime token of %s:\n%s\n", attach, registered.Name, registered.RuntimeToken)
	command.WarnLocalOnly(stderr, "the API address", registered.APIURL)
	if !*bareMetal {
		command.WarnLocalOnly(stderr, "the metrics address", registered.MetricsURL)
	}
	command.WarnLocalOnly(stderr, "the image registry", registered.Registry)
	return 0
}

func ClusterToken(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster token", `usage: hl cluster token NAME [--org ORG]

Prints the runtime token of a cluster the org registered, and nothing else. The token is derived
from the cluster's name and never changes: removing the cluster is what ends it. The command
hl cluster add prints asks for it on stdin, so pipe it there and keep it out of a command line.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	// Asking for a token registers nothing: the name must be a cluster the org already has.
	var listing struct {
		Clusters []struct {
			Name  string `json:"name"`
			Owned bool   `json:"owned"`
		} `json:"clusters"`
	}
	if err := c.Do("GET", orgPath(id, "/clusters"), nil, &listing); err != nil {
		return command.Fail(stderr, err)
	}
	owned := false
	for _, cluster := range listing.Clusters {
		owned = owned || (cluster.Name == name && cluster.Owned)
	}
	if !owned {
		return command.Fail(stderr, fmt.Errorf("%s is not a cluster org %s registered: see hl cluster list, or register it with hl cluster add", name, id))
	}
	var registered registeredCluster
	if err := c.Do("POST", orgPath(id, "/clusters"), map[string]string{"name": name}, &registered); err != nil {
		return command.Fail(stderr, err)
	}
	fmt.Fprintln(stdout, registered.RuntimeToken)
	return 0
}

func ClusterForgetIdentity(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster forget-identity", `usage: hl cluster forget-identity NAME [--org ORG]

A cluster is bound to the first cluster id its runtime reports, and a runtime reporting another is
refused. After the machines behind a name are rebuilt, run this: the cluster binds to the id its
runtime reports next. The cluster keeps its name, its place in the order and its settings.`, stderr)
	return onCluster(c, fs, args, "DELETE", "/identity", stdout, stderr)
}

func ClusterOrder(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster order", `usage: hl cluster order NAME... [--org ORG]

Sets the order the org's jobs are placed on the clusters it registered: name every one of them
once, first placed first. A job goes to the first cluster that can take it as the scheduler reads
capacity at that moment, and to a later one when that is full or has just failed it. A guaranteed
job first asks an earlier cluster that scales up to do so, and takes a later one when that times
out. A cluster registered afterwards comes last until it is ordered, and the clusters the platform
shares come after all of the org's own. Jobs that are placed do not move. Prints the clusters in
the new order, as hl cluster list does.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	names, err := command.ParseOperands(fs, args)
	if err != nil {
		return command.ParseExit(err)
	}
	if len(names) == 0 {
		return command.ParseExit(command.Misuse(fs, "name the org's clusters in the order to place jobs on them"))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "PUT", orgPath(id, "/clusters/order"), map[string][]string{"names": names}, "clusters", "", stdout, stderr)
}

func ClusterSettings(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster settings", `usage: hl cluster settings NAME [--scale-up-timeout SECONDS] [--speculative-cap N] [--stuck-pending-timeout SECONDS] [--status-silence-ceiling SECONDS] [--org ORG]

Sets what a cluster the org registered uses in place of the scheduler's defaults. Most matter for a
cluster whose nodes appear when a job asks for them, --status-silence-ceiling for one whose link to
the platform drops while its jobs run on. Only the flags given change, and 0 puts one back on the
default. hl cluster list shows the ones that are set.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	body := map[string]int{}
	for flag, field := range map[string]string{
		"scale-up-timeout":       "scale_up_timeout_seconds",
		"speculative-cap":        "max_speculative_accelerators",
		"stuck-pending-timeout":  "stuck_pending_timeout_seconds",
		"status-silence-ceiling": "status_silence_ceiling_seconds",
	} {
		fs.Func(flag, clusterSettingHelp[flag], func(text string) error {
			n, err := strconv.Atoi(text)
			if err != nil || n < 0 {
				return fmt.Errorf("want a whole number, or 0 for the default")
			}
			body[field] = n
			return nil
		})
	}
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	if len(body) == 0 {
		return command.ParseExit(command.Misuse(fs, "name a setting to change"))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "PATCH", orgPath(id, "/clusters/"+url.PathEscape(name)), body, "", "", stdout, stderr)
}

var clusterSettingHelp = map[string]string{
	"scale-up-timeout":       "seconds a job placed ahead of capacity waits for the cluster's autoscaler to bring a node, under 1800",
	"speculative-cap":        "the most accelerators that jobs placed ahead of capacity hold on the cluster at once",
	"stuck-pending-timeout":  "seconds a job admitted to the cluster may stay unplaced before it is evicted as stuck_pending",
	"status-silence-ceiling": "seconds the cluster may report no job status before the jobs it holds are evicted as cluster_unreachable",
}

func ClusterDisable(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster disable", `usage: hl cluster disable NAME [--org ORG]

Turns a cluster off for the org's jobs: from now on none of them is placed on it. The cluster is
any that hl cluster list shows, one the org registered or one the platform shares, and the
decision is this org's alone. Its jobs already placed there are not touched and run to their
end. A job that waits because the org turned off every cluster it could use reads
not_admitted_reason clusters_disabled. Turn it on again with hl cluster enable.`, stderr)
	return onCluster(c, fs, args, "POST", "/disable", stdout, stderr)
}

func ClusterEnable(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster enable", `usage: hl cluster enable NAME [--org ORG]

Turns a cluster on again for the org's jobs after hl cluster disable, which is how every cluster
starts.`, stderr)
	return onCluster(c, fs, args, "POST", "/enable", stdout, stderr)
}

func ClusterRemove(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster remove", `usage: hl cluster remove NAME [--org ORG]

Ends the cluster. Every job placed on it goes back to the queue, keeping its retries, and is
placed on another cluster that takes its org; requeued lists them. The cluster's settings are
deleted and its runtime token is refused from then on. Its agent reads that, stops the jobs it
still runs and exits. The name becomes free for any org to register.`, stderr)
	return onCluster(c, fs, args, "DELETE", "", stdout, stderr)
}

// onCluster calls one operation of a cluster of the org and prints what it answers.
func onCluster(c *apiclient.Client, fs *command.Flags, args []string, method, rest string, stdout, stderr io.Writer) int {
	org := fs.String("org", "", orgFlagHelp)
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, method, orgPath(id, "/clusters/"+url.PathEscape(name)+rest), nil, "", "", stdout, stderr)
}

func ClusterList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("cluster list", `usage: hl cluster list [--org ORG]

The clusters the org's jobs run on, by name: the ones it registered (owned) and the ones the
platform shares with every org, in the order a job is placed on them (see hl cluster order),
with whether an agent is connected now and whether the org has the cluster on for its jobs
(enabled; see hl cluster disable). last_contact_at is when its agent was last heard from, within
30 days. Of a cluster the org registered: agent_version, the image tag its agent runs, with
version_skew when that is not the platform's own, and last_error, what the platform last refused
its agent with. A cluster with version_skew, or not connected with a last_error, is also warned
about on stderr.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	var listing struct {
		Clusters json.RawMessage `json:"clusters"`
	}
	if err := c.Do("GET", orgPath(id, "/clusters"), nil, &listing); err != nil {
		return command.Fail(stderr, err)
	}
	if err := command.PrintLines(stdout, stderr, "cluster list: none", listing.Clusters); err != nil {
		return command.Fail(stderr, err)
	}
	var clusters []struct {
		Name         string `json:"name"`
		Connected    bool   `json:"connected"`
		AgentVersion string `json:"agent_version"`
		VersionSkew  bool   `json:"version_skew"`
		LastError    string `json:"last_error"`
	}
	if err := json.Unmarshal(listing.Clusters, &clusters); err != nil {
		return command.Fail(stderr, fmt.Errorf("response is not a list of clusters: %w", err))
	}
	for _, cluster := range clusters {
		if cluster.VersionSkew {
			fmt.Fprintf(stderr, "hl: warning: cluster %s runs agent version %s, not the platform's: run its attach command again (hl cluster add %s) to upgrade it\n",
				cluster.Name, cluster.AgentVersion, cluster.Name)
		}
		if !cluster.Connected && cluster.LastError != "" {
			fmt.Fprintf(stderr, "hl: warning: cluster %s is not connected, and its agent was last refused with: %s\n", cluster.Name, cluster.LastError)
		}
	}
	return 0
}
