package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
)

// The account commands: orgs, projects, API keys and clusters. --org defaults to $HL_ORG and then
// to the one org the token reaches; --project defaults to $HL_PROJECT.

var orgCommands = []subcommand{
	{"list", "the orgs this token reaches, with your role in each", orgList},
	{"create", "create an org you own, with its default project", orgCreate},
	{"rename", "rename an org; its id stays", orgRename},
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
	{"list", "the clusters an org's jobs run on and whether each is connected", clusterList},
}

const orgFlagHelp = "org (default $HL_ORG, then the one org this token reaches)"

// resolveOrg is the org a command acts in: the flag, else $HL_ORG, else the only org the token
// reaches.
func resolveOrg(c *apiclient.Client, flagValue string) (string, error) {
	if org := orEnv(flagValue, orgEnv); org != "" {
		return org, nil
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

Creates an org with a project named default. A signed-in person owns the org they create. A
staff token names the owner with --owner. An API key belongs to one org and cannot create another.`, stderr)
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
	fs := newFlags("project create", "usage: hl project create NAME [--org ORG]\n\nCreates a project. Its name is unique within the org. Export its id as $HL_PROJECT to work in it.", stderr)
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

func clusterAdd(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster add", `usage: hl cluster add NAME [--org ORG]

Registers the cluster name for the org and prints the command that installs its agent, with the
cluster's runtime token and the addresses its agent reaches the platform at filled in. Run that
command with kubectl pointed at the cluster. The token is derived from the name, so adding a name
the org already has prints the same command again. A cluster takes its own org's jobs only.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := parseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return parseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return fail(stderr, err)
	}
	var registered struct {
		Name         string `json:"name"`
		RuntimeToken string `json:"runtime_token"`
		APIURL       string `json:"api_url"`
		MetricsURL   string `json:"metrics_url"`
	}
	if err := c.Do("POST", orgPath(id, "/clusters"), map[string]string{"name": name}, &registered); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "RUNTIME_TOKEN=%s make cluster-agent-up CLUSTER=%s API_URL=%s METRICS_URL=%s\n",
		registered.RuntimeToken, registered.Name, registered.APIURL, registered.MetricsURL)
	return 0
}

func clusterList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("cluster list", "usage: hl cluster list [--org ORG]\n\nThe clusters the org's jobs run on, by name: the ones it registered (owned) and the ones the platform shares with every org, with whether an agent is connected now.", stderr)
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
