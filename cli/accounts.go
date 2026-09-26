package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

// The account commands: orgs with their members and invites, projects, API keys and clusters. --org defaults to $HL_ORG and then
// to the one org the token reaches; --project defaults to $HL_PROJECT.

var orgCommands = []subcommand{
	{"list", "the orgs this token reaches, with your role in each", orgList},
	{"create", "create an org you own, with its default project", orgCreate},
	{"rename", "rename an org; its id stays", orgRename},
	{"members", "the people of an org: list them, change a role, remove one or leave", orgMembers},
	{"invites", "invite people into an org by a token, and accept one", orgInvites},
}

var orgMemberCommands = []subcommand{
	{"list", "an org's members and the role each holds", orgMembersList},
	{"set-role", "make a member an owner, or an owner a member", orgMembersSetRole},
	{"remove", "remove a member, or leave by naming yourself", orgMembersRemove},
}

var orgInviteCommands = []subcommand{
	{"create", "create an invite; its token is printed once and never again", orgInvitesCreate},
	{"list", "an org's pending invites, never their tokens", orgInvitesList},
	{"revoke", "end an invite that was not accepted", orgInvitesRevoke},
	{"accept", "join the org an invite is for, as the person signed in", orgInvitesAccept},
}

func orgMembers(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	return runGroup("org members", orgMemberCommands, c, args, stdout, stderr)
}

func orgInvites(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	return runGroup("org invites", orgInviteCommands, c, args, stdout, stderr)
}

var projectCommands = []subcommand{
	{"list", "an org's projects", projectList},
	{"create", "create a project in an org", projectCreate},
	{"rename", "rename a project; its id stays", projectRename},
}

var apiKeyCommands = []subcommand{
	{"create", "create an API key; its token is printed once and never again", apiKeyCreate},
	{"list", "an org's API keys, revoked ones included, never their tokens", apiKeyList},
	{"revoke", "end an API key and every key minted from it", apiKeyRevoke},
}

var clusterCommands = []subcommand{
	{"add", "register a cluster for an org and print the command that starts its agent", clusterAdd},
	{"list", "the clusters an org's jobs run on, whether each is connected and whether the org has it enabled", clusterList},
	{"token", "print the runtime token of a cluster the org registered", clusterToken},
	{"priority", "set where a cluster the org registered stands among the ones its jobs are placed on, 1 to 1000, higher first", clusterPriority},
	{"disable", "turn a cluster off for the org's jobs: none is placed there, the ones running finish", clusterDisable},
	{"enable", "turn a cluster on again for the org's jobs", clusterEnable},
	{"forget-identity", "let a cluster whose machines were rebuilt bind to the identity they report now", clusterForgetIdentity},
	{"remove", "end a cluster: requeue its jobs and refuse its runtime token", clusterRemove},
}

const orgFlagHelp = "org (default $HL_ORG, then the one org this token reaches)"

// resolveOrg is the org a command acts in: the flag, else $HL_ORG, else the only org the token
// reaches.
func resolveOrg(c *apiclient.Client, flagValue string) (string, error) {
	if org := orEnv(flagValue, orgEnv); org != "" {
		return org, nil
	}
	if kind := kindOf(c.Token); kind != callerOther {
		return "", errors.New("this is a command of an org's people, and HL_TOKEN is the token of an agent or a coordinator of one platform experiment, which reaches no org. " +
			"hl capacity shows the clusters its jobs can run on, hl platform-experiments get its experiment")
	}
	var me struct {
		Orgs []struct {
			ID string `json:"id"`
		} `json:"orgs"`
	}
	if err := c.Do("GET", "/me", nil, &me); err != nil {
		return "", err
	}
	if len(me.Orgs) != 1 {
		return "", fmt.Errorf("this token reaches %d orgs: name one with --org or $HL_ORG (hl org list shows them)", len(me.Orgs))
	}
	return me.Orgs[0].ID, nil
}

// call makes one request and prints its reply, an object or the list under listKey, one compact
// line each.
func call(c *apiclient.Client, method, path string, body any, listKey, none string, stdout, stderr io.Writer) int {
	var out json.RawMessage
	if err := c.Do(method, path, body, &out); err != nil {
		return fail(stderr, err)
	}
	if listKey != "" {
		var keyed map[string]json.RawMessage
		if err := json.Unmarshal(out, &keyed); err != nil {
			return fail(stderr, fmt.Errorf("response is not a JSON object: %w", err))
		}
		out = keyed[listKey]
	}
	if err := printLines(stdout, stderr, none, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func orgPath(org, rest string) string {
	return "/orgs/" + url.PathEscape(org) + rest
}

func orgList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org list", "usage: hl org list\n\nThe orgs this token reaches, one per line: id, name and, for a person, their role.", stderr)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	return call(c, "GET", "/me", nil, "orgs", "org list: this token reaches no org", stdout, stderr)
}

func orgCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org create", `usage: hl org create NAME [--owner USER]

Creates an org with a project named Default project. A signed-in person owns the org they create. A
staff token names the owner with --owner. An API key belongs to one org and cannot create another.
A person owns a bounded number of orgs, the first one they signed in to included: past it the
create is refused with max_orgs_reached.`, stderr)
	owner := fs.String("owner", "", "user id of the owner, for a staff token")
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	body := map[string]string{"name": name}
	if *owner != "" {
		body["owner_user_id"] = *owner
	}
	return call(c, "POST", "/orgs", body, "", "", stdout, stderr)
}

func orgRename(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org rename", "usage: hl org rename NAME [--org ORG]\n\nGives the org a new name. Its id, projects, keys and clusters stay.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "PATCH", orgPath(id, ""), map[string]string{"name": name}, "", "", stdout, stderr)
}

// done makes a request that answers with no body and says on stderr what it did, so that no
// output is never mistaken for a command that did nothing.
func done(c *apiclient.Client, method, path, said string, stderr io.Writer) int {
	if err := c.Do(method, path, nil, nil); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stderr, "hl: %s\n", said)
	return 0
}

// pageFlags are --limit and --offset of a list the platform pages.
func pageFlags(fs *flagSet) func() string {
	limit := fs.Int("limit", 0, "page size (default the platform's 20, at most 200)")
	offset := fs.Int("offset", 0, "rows to skip")
	return func() string {
		query := url.Values{}
		if *limit != 0 {
			query.Set("limit", fmt.Sprint(*limit))
		}
		if *offset != 0 {
			query.Set("offset", fmt.Sprint(*offset))
		}
		if len(query) == 0 {
			return ""
		}
		return "?" + query.Encode()
	}
}

func orgMembersList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org members list", `usage: hl org members list [--org ORG] [--limit N] [--offset N]

The org's members, oldest first: user id, role and when they joined. The platform holds no name
and no email for a person, so a member is named by user id. An owner changes everything in the
org; a member reads everything, runs platform experiments and creates API keys bound to one
project.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	query := pageFlags(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "GET", orgPath(id, "/members")+query(), nil, "", "org members list: none", stdout, stderr)
}

func orgMembersSetRole(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org members set-role", `usage: hl org members set-role USER owner|member [--org ORG]

For an owner. An org keeps at least one owner, so demoting the last one is refused. Someone who
stops being an owner loses their API keys of the whole org and the invites they made.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) != 2 {
		return parseExit(misuse(fs, "want USER and a role, got %d operands", len(operands)))
	}
	if operands[1] != "owner" && operands[1] != "member" {
		return parseExit(misuse(fs, "the role must be owner or member, got %s", operands[1]))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "PATCH", orgPath(id, "/members/"+url.PathEscape(operands[0])), map[string]string{"role": operands[1]}, "", "", stdout, stderr)
}

func orgMembersRemove(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org members remove", `usage: hl org members remove USER [--org ORG]

For an owner, and for anyone naming themselves, which is how a person leaves an org. The last
owner cannot be removed. The API keys the person created in the org stop working at once and the
invites they made are revoked.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	user, err := parseOne(fs, args, "USER", "", "user")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return done(c, "DELETE", orgPath(id, "/members/"+url.PathEscape(user)), fmt.Sprintf("removed %s from org %s", user, id), stderr)
}

func orgInvitesCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org invites create", `usage: hl org invites create [--role owner|member] [--org ORG]

For an owner. Prints the invite with its token. The token is shown this once: the platform keeps
only its hash and sends no email, so hand it to the person yourself. Whoever signs in and accepts
it joins the org with the invite's role. It works once, for seven days, and until it is revoked.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	role := fs.String("role", "member", "the role the invited person joins with")
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	if *role != "owner" && *role != "member" {
		return parseExit(misuse(fs, "--role must be owner or member, got %s", *role))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "POST", orgPath(id, "/invites"), map[string]string{"role": *role}, "", "", stdout, stderr)
}

func orgInvitesList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org invites list", "usage: hl org invites list [--org ORG] [--limit N] [--offset N]\n\nFor an owner. The invites that can still be accepted, newest first.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	query := pageFlags(fs)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "GET", orgPath(id, "/invites")+query(), nil, "", "org invites list: none", stdout, stderr)
}

func orgInvitesRevoke(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org invites revoke", "usage: hl org invites revoke ID\n\nFor an owner of the invite's org. The invite can no longer be accepted. One already accepted is refused: remove the member instead.", stderr)
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	return done(c, "DELETE", "/invites/"+url.PathEscape(id), "revoked invite "+id, stderr)
}

func orgInvitesAccept(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("org invites accept", `usage: hl org invites accept TOKEN

Joins the org the invite is for, with the invite's role. Only a signed-in person joins an org, so
HL_TOKEN must be your own sign-in: an API key is refused. Prints the membership.`, stderr)
	token, err := parseOne(fs, args, "TOKEN", "", "token")
	if err != nil {
		return parseExit(err)
	}
	return call(c, "POST", "/invites/accept", map[string]string{"token": token}, "", "", stdout, stderr)
}

func projectList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("project list", "usage: hl project list [--org ORG]\n\nThe org's projects, oldest first. An API key bound to one project sees that one.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "GET", orgPath(id, "/projects"), nil, "projects", "project list: none", stdout, stderr)
}

func projectCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("project create", "usage: hl project create NAME [--org ORG]\n\nCreates a project. Its name is unique within the org. Export its id as $HL_PROJECT to work in it. An org holds a bounded number of projects, its default one included: past it the create is refused with max_projects_reached.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "POST", orgPath(id, "/projects"), map[string]string{"name": name}, "", "", stdout, stderr)
}

func projectRename(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("project rename", "usage: hl project rename NAME [--project PROJECT]\n\nGives the project a new name. Its id and its platform experiments stay.", stderr)
	project := fs.String("project", "", "project id (default $HL_PROJECT)")
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	if *project = orEnv(*project, projectEnv); *project == "" {
		return parseExit(misuse(fs, "--project or $HL_PROJECT is required"))
	}
	return call(c, "PATCH", "/projects/"+url.PathEscape(*project), map[string]string{"name": name}, "", "", stdout, stderr)
}

func apiKeyCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("api-key create", `usage: hl api-key create NAME [--project PROJECT] [--org ORG]

Creates an API key and prints it with its token. The token is shown this once: the platform
keeps only its hash. Without --project the key reaches the whole org; with it, that project
only. $HL_PROJECT is not read here, so a key is never narrowed by accident. A key may mint keys
no wider than itself, and revoking a key revokes every key minted from it.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	project := fs.String("project", "", "bind the key to this one project")
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	body := map[string]string{"name": name}
	if *project != "" {
		body["project_id"] = *project
	}
	return call(c, "POST", orgPath(id, "/api-keys"), body, "", "", stdout, stderr)
}

func apiKeyList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("api-key list", "usage: hl api-key list [--org ORG]\n\nThe org's API keys, newest first, revoked ones with revoked_at. An API key sees itself and the keys minted from it.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "GET", orgPath(id, "/api-keys"), nil, "api_keys", "api-key list: none", stdout, stderr)
}

func apiKeyRevoke(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("api-key revoke", "usage: hl api-key revoke ID\n\nEnds the key and every key minted from it, at once.", stderr)
	id, err := parseOne(fs, args, "ID", "", "id")
	if err != nil {
		return parseExit(err)
	}
	return call(c, "DELETE", "/api-keys/"+url.PathEscape(id), nil, "", "", stdout, stderr)
}

// registeredCluster is what registering a cluster answers.
type registeredCluster struct {
	Name         string `json:"name"`
	RuntimeToken string `json:"runtime_token"`
	APIURL       string `json:"api_url"`
	MetricsURL   string `json:"metrics_url"`
	Registry     string `json:"registry"`
	ImageTag     string `json:"image_tag"`
	JobImage     string `json:"job_image"`
}

func clusterAdd(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster add", `usage: hl cluster add NAME [--bare-metal] [--org ORG]

Registers the cluster name for the org and prints the one command that attaches it, complete:
the addresses its agent reaches the platform at, the registry and tag of its images and the
platform's job image, where a job that names no image runs. Below the command it prints the
cluster's runtime token, which the command asks for when it runs, so the token is in no shell
history and the machine needs neither hl nor an API key. Adding a name the org already has
prints the same again. A cluster takes its own org's jobs only.

Without --bare-metal the command is for a Kubernetes cluster: run it with helm 3.17 or newer
and kubectl pointed at the cluster (add --kube-context to name another). It installs the
runtime's chart from the registry, or upgrades it, and takes over a hypothesisloop-jobs
namespace an older install left. helm uninstall hypothesisloop-cluster-agent -n hypothesisloop
detaches the cluster again: it removes the cluster here, where its jobs go back to the queue,
and stops what it runs there.

With --bare-metal it is for one node with docker or podman, which is a cluster of its own. Run
the command on the node. It copies the agent out of its image and installs it as a systemd
unit that starts at boot. It needs root, outbound access to the platform and the image
registries, and nothing inbound. Running the command of a newer image tag upgrades the agent.
To detach the node again, on the node:

  sudo bare-agent detach        removes the cluster here, stops its jobs and the agent
  sudo bare-agent uninstall     removes the agent from the node

hl cluster remove NAME detaches from this side: the agent stops its jobs and stays stopped.`, stderr)
	bareMetal := fs.Bool("bare-metal", false, "print the command for a bare-metal node")
	org := fs.String("org", "", orgFlagHelp)
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	var registered registeredCluster
	if err := c.Do("POST", orgPath(id, "/clusters"), map[string]string{"name": name}, &registered); err != nil {
		return fail(stderr, err)
	}
	command := kubernetesCommand(registered)
	if *bareMetal {
		command = bareMetalCommand(registered)
	}
	fmt.Fprintf(stdout, "%s\n\nIt asks for the runtime token of %s:\n%s\n", command, registered.Name, registered.RuntimeToken)
	return 0
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
func kubernetesCommand(registered registeredCluster) string {
	set := func(key, value string) string {
		return " --set-string " + shellQuote(key+"="+helmValue(value))
	}
	return fmt.Sprintf("sh -c %s sh %s %s", shellQuote(kubernetesScript),
		shellQuote("oci://"+registered.Registry+"/charts/hypothesisloop-cluster-agent"), shellQuote("0.1.0+"+registered.ImageTag)) +
		set("clusterName", registered.Name) + set("apiURL", registered.APIURL) + set("metricsURL", registered.MetricsURL) +
		set("images.registry", registered.Registry) + set("images.tag", registered.ImageTag) + set("jobImage", registered.JobImage)
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
// name, $2 the API address, $3 the image that carries the agent and $4 the job image.
const bareMetalScript = `engine="$(command -v docker || command -v podman)" || { echo "docker or podman is needed on this node" >&2; exit 1; }; ` +
	`staged="$("$engine" create "$3")" && "$engine" cp "$staged:/usr/local/bin/bare-agent" /usr/local/bin/bare-agent.new && "$engine" rm "$staged" >/dev/null && ` +
	`exec /usr/local/bin/bare-agent.new install --cluster "$1" --api-url "$2" --job-image "$4"`

// bareMetalCommand installs the agent of a bare-metal node and holds no token.
func bareMetalCommand(registered registeredCluster) string {
	return fmt.Sprintf("sudo sh -c %s sh %s %s %s %s", shellQuote(bareMetalScript), shellQuote(registered.Name), shellQuote(registered.APIURL),
		shellQuote(registered.Registry+"/hypothesisloop-bare-agent:"+registered.ImageTag), shellQuote(registered.JobImage))
}

// shellQuote writes text as one word of a POSIX shell command, whatever it holds.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

func clusterToken(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster token", `usage: hl cluster token NAME [--org ORG]

Prints the runtime token of a cluster the org registered, and nothing else. The token is derived
from the cluster's name and never changes: removing the cluster is what ends it. Meant for
command substitution, as in the command hl cluster add prints.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	// Asking for a token registers nothing: the name must be a cluster the org already has.
	var listing struct {
		Clusters []struct {
			Name  string `json:"name"`
			Owned bool   `json:"owned"`
		} `json:"clusters"`
	}
	if err := c.Do("GET", orgPath(id, "/clusters"), nil, &listing); err != nil {
		return fail(stderr, err)
	}
	owned := false
	for _, cluster := range listing.Clusters {
		owned = owned || (cluster.Name == name && cluster.Owned)
	}
	if !owned {
		return fail(stderr, fmt.Errorf("%s is not a cluster org %s registered: see hl cluster list, or register it with hl cluster add", name, id))
	}
	var registered registeredCluster
	if err := c.Do("POST", orgPath(id, "/clusters"), map[string]string{"name": name}, &registered); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, registered.RuntimeToken)
	return 0
}

func clusterForgetIdentity(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster forget-identity", `usage: hl cluster forget-identity NAME [--org ORG]

A cluster is bound to the first cluster id its runtime reports, and a runtime reporting another is
refused. After the machines behind a name are rebuilt, run this: the cluster binds to the id its
runtime reports next. The settings kept under the old id are deleted.`, stderr)
	return onCluster(c, fs, args, "DELETE", "/identity", stdout, stderr)
}

func clusterDisable(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster disable", `usage: hl cluster disable NAME [--org ORG]

Turns a cluster off for the org's jobs: from now on none of them is placed on it. The cluster is
any that hl cluster list shows, one the org registered or one the platform shares, and the
decision is this org's alone. Its jobs already placed there are not touched and run to their
end. A job that waits because the org turned off every cluster it could use reads
not_admitted_reason clusters_disabled. Turn it on again with hl cluster enable.`, stderr)
	return onCluster(c, fs, args, "POST", "/disable", stdout, stderr)
func clusterPriority(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster priority", `usage: hl cluster priority NAME VALUE [--org ORG]

Sets where a cluster the org registered stands among the ones its jobs are placed on: 1 to 1000,
higher first, 100 until it is set. A job goes to the highest-priority cluster that can take it as
the scheduler reads capacity at that moment, and to a lower one when that is full or has just
failed it. A guaranteed job first asks a higher cluster that scales up to do so, and takes a lower
one when that times out. A cluster the platform shares holds 0 and takes no other, so an org's
own clusters are tried before it. Jobs that are placed do not move. hl cluster list shows the
priorities, in the order placement goes by.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	operands, err := parseOperands(fs, args)
	if err != nil {
		return parseExit(err)
	}
	if len(operands) != 2 {
		return parseExit(misuse(fs, "want NAME and a priority, got %d operands", len(operands)))
	}
	priority, err := strconv.Atoi(operands[1])
	if err != nil {
		return parseExit(misuse(fs, "the priority must be a whole number, got %s", operands[1]))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "PATCH", orgPath(id, "/clusters/"+url.PathEscape(operands[0])), map[string]int{"priority": priority}, "", "", stdout, stderr)
}

}

func clusterEnable(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster enable", `usage: hl cluster enable NAME [--org ORG]

Turns a cluster on again for the org's jobs after hl cluster disable, which is how every cluster
starts.`, stderr)
	return onCluster(c, fs, args, "POST", "/enable", stdout, stderr)
}

func clusterRemove(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster remove", `usage: hl cluster remove NAME [--org ORG]

Ends the cluster. Every job placed on it goes back to the queue, keeping its retries, and is
placed on another cluster that takes its org; requeued lists them. The cluster's settings are
deleted and its runtime token is refused from then on. Its agent reads that, stops the jobs it
still runs and exits. The name becomes free for any org to register.`, stderr)
	return onCluster(c, fs, args, "DELETE", "", stdout, stderr)
}

// onCluster calls one operation of a cluster of the org and prints what it answers.
func onCluster(c *apiclient.Client, fs *flagSet, args []string, method, rest string, stdout, stderr io.Writer) int {
	org := fs.String("org", "", orgFlagHelp)
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, method, orgPath(id, "/clusters/"+url.PathEscape(name)+rest), nil, "", "", stdout, stderr)
}

func clusterList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster list", "usage: hl cluster list [--org ORG]\n\nThe clusters the org's jobs run on, by name: the ones it registered (owned) and the ones the platform shares with every org, in the order a job is placed on them (priority, then name; see hl cluster priority), with whether an agent is connected now and whether the org has the cluster on for its jobs (enabled; see hl cluster disable).", stderr)
	org := fs.String("org", "", orgFlagHelp)
	if err := parseNone(fs, args); err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	return call(c, "GET", orgPath(id, "/clusters"), nil, "clusters", "cluster list: none", stdout, stderr)
}

// parseNone parses the flags of a command that takes no operand.
func parseNone(fs *flagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return misuse(fs, "unexpected argument %q", fs.Arg(0))
	}
	return nil
}
