package people

import (
	"fmt"
	"io"
	"net/url"
	"slices"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/contract/platformapi"

	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

// The account commands: orgs with their members and invites, projects, API keys and clusters. --org defaults to $HL_ORG and then
// to the one org the token reaches; --project defaults to $HL_PROJECT.

const orgFlagHelp = "org (default $HL_ORG, then the one org this token reaches)"

// resolveOrg is the org a command acts in: the flag, else $HL_ORG, else the only org the token
// reaches.
func resolveOrg(c *apiclient.Client, flagValue string) (string, error) {
	if org := command.OrEnv(flagValue, command.OrgEnv); org != "" {
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

func orgPath(org, rest string) string {
	return "/orgs/" + url.PathEscape(org) + rest
}

func OrgList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org list", "usage: hl org list\n\nThe orgs this token reaches, one per line: id, name and, for a person, their role.", stderr)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	return command.Call(c, "GET", "/me", nil, "orgs", "org list: this token reaches no org", stdout, stderr)
}

func OrgCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org create", `usage: hl org create NAME [--owner USER]

Creates an org with a project named Default project. A signed-in person owns the org they create. A
staff token names the owner with --owner. An API key belongs to one org and cannot create another.
A person owns a bounded number of orgs, the first one they signed in to included: past it the
create is refused with max_orgs_reached.`, stderr)
	owner := fs.String("owner", "", "user id of the owner, for a staff token")
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	body := map[string]string{"name": name}
	if *owner != "" {
		body["owner_user_id"] = *owner
	}
	return command.Call(c, "POST", "/orgs", body, "", "", stdout, stderr)
}

func OrgRename(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org rename", "usage: hl org rename NAME [--org ORG]\n\nGives the org a new name. Its id, projects, keys and clusters stay.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "PATCH", orgPath(id, ""), map[string]string{"name": name}, "", "", stdout, stderr)
}

func OrgMembersList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org members list", `usage: hl org members list [--org ORG] [--limit N] [--offset N]

The org's members, oldest first: user id, role and when they joined. The platform holds no name
and no email for a person, so a member is named by user id. An owner changes everything in the
org; a member reads everything, runs platform experiments and creates API keys bound to one
project.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	query, offset := command.PageFlags(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.CallPage(c, orgPath(id, "/members")+query(), "org members list", *offset, stdout, stderr)
}

func OrgMembersSetRole(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org members set-role", `usage: hl org members set-role USER owner|member [--org ORG]

For an owner. An org keeps at least one owner, so demoting the last one is refused. Someone who
stops being an owner loses their API keys of the whole org and the invites they made.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	operands, err := command.ParseOperands(fs, args)
	if err != nil {
		return command.ParseExit(err)
	}
	if len(operands) != 2 {
		return command.ParseExit(command.Misuse(fs, "want USER and a role, got %d operands", len(operands)))
	}
	if !slices.Contains(platformapi.OrgRoles, platformapi.OrgRole(operands[1])) {
		return command.ParseExit(command.Misuse(fs, "the role must be %s or %s, got %s", platformapi.OrgRoleOwner, platformapi.OrgRoleMember, operands[1]))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "PATCH", orgPath(id, "/members/"+url.PathEscape(operands[0])), map[string]string{"role": operands[1]}, "", "", stdout, stderr)
}

func OrgMembersRemove(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org members remove", `usage: hl org members remove USER [--org ORG]

For an owner, and for anyone naming themselves, which is how a person leaves an org. The last
owner cannot be removed. The API keys the person created in the org stop working at once and the
invites they made are revoked.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	user, err := command.ParseOne(fs, args, "USER", "", "user")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "DELETE", orgPath(id, "/members/"+url.PathEscape(user)), nil, "", fmt.Sprintf("removed %s from org %s", user, id), stdout, stderr)
}

func OrgInvitesCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org invites create", `usage: hl org invites create [--role owner|member] [--org ORG]

For an owner. Prints the invite with its token. The token is shown this once: the platform keeps
only its hash and sends no email, so hand it to the person yourself. Whoever signs in and accepts
it joins the org with the invite's role. It works once, for seven days, and until it is revoked.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	role := fs.String("role", string(platformapi.OrgRoleMember), "the role the invited person joins with")
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	if !slices.Contains(platformapi.OrgRoles, platformapi.OrgRole(*role)) {
		return command.ParseExit(command.Misuse(fs, "--role must be %s or %s, got %s", platformapi.OrgRoleOwner, platformapi.OrgRoleMember, *role))
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "POST", orgPath(id, "/invites"), map[string]string{"role": *role}, "", "", stdout, stderr)
}

func OrgInvitesList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org invites list", "usage: hl org invites list [--org ORG] [--limit N] [--offset N]\n\nFor an owner. The invites that can still be accepted, newest first.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	query, offset := command.PageFlags(fs)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.CallPage(c, orgPath(id, "/invites")+query(), "org invites list", *offset, stdout, stderr)
}

func OrgInvitesRevoke(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org invites revoke", "usage: hl org invites revoke ID\n\nFor an owner of the invite's org. The invite can no longer be accepted. One already accepted is refused: remove the member instead.", stderr)
	id, err := command.ParseOne(fs, args, "ID", "", "id")
	if err != nil {
		return command.ParseExit(err)
	}
	return command.Call(c, "DELETE", "/invites/"+url.PathEscape(id), nil, "", "revoked invite "+id, stdout, stderr)
}

func OrgInvitesAccept(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("org invites accept", `usage: hl org invites accept TOKEN

Joins the org the invite is for, with the invite's role. Only a signed-in person joins an org, so
HL_TOKEN must be your own sign-in: an API key is refused. Prints the membership.`, stderr)
	token, err := command.ParseOne(fs, args, "TOKEN", "", "token")
	if err != nil {
		return command.ParseExit(err)
	}
	return command.Call(c, "POST", "/invites/accept", map[string]string{"token": token}, "", "", stdout, stderr)
}

func ProjectList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("project list", "usage: hl project list [--org ORG]\n\nThe org's projects, oldest first. An API key bound to one project sees that one.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "GET", orgPath(id, "/projects"), nil, "projects", "project list: none", stdout, stderr)
}

func ProjectCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("project create", "usage: hl project create NAME [--org ORG]\n\nCreates a project. Its name is unique within the org. Export its id as $HL_PROJECT to work in it. An org holds a bounded number of projects, its default one included: past it the create is refused with max_projects_reached.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "POST", orgPath(id, "/projects"), map[string]string{"name": name}, "", "", stdout, stderr)
}

func ProjectRename(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("project rename", "usage: hl project rename NAME [--project PROJECT]\n\nGives the project a new name. Its id and its platform experiments stay.", stderr)
	project := fs.String("project", "", "project id (default $HL_PROJECT)")
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	if *project = command.OrEnv(*project, command.ProjectEnv); *project == "" {
		return command.ParseExit(command.Misuse(fs, "--project or $HL_PROJECT is required"))
	}
	return command.Call(c, "PATCH", "/projects/"+url.PathEscape(*project), map[string]string{"name": name}, "", "", stdout, stderr)
}

func ApiKeyCreate(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("api-key create", `usage: hl api-key create NAME [--project PROJECT] [--org ORG]

Creates an API key and prints it with its token. The token is shown this once: the platform
keeps only its hash. Without --project the key reaches the whole org; with it, that project
only. $HL_PROJECT is not read here, so a key is never narrowed by accident. A key may mint keys
no wider than itself, and revoking a key revokes every key minted from it.`, stderr)
	org := fs.String("org", "", orgFlagHelp)
	project := fs.String("project", "", "bind the key to this one project")
	name, err := command.ParseOne(fs, args, "NAME", "", "name")
	if err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	body := map[string]string{"name": name}
	if *project != "" {
		body["project_id"] = *project
	}
	return command.Call(c, "POST", orgPath(id, "/api-keys"), body, "", "", stdout, stderr)
}

func ApiKeyList(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("api-key list", "usage: hl api-key list [--org ORG]\n\nThe org's API keys, newest first, revoked ones with revoked_at. An API key sees itself and the keys minted from it.", stderr)
	org := fs.String("org", "", orgFlagHelp)
	if err := command.ParseNone(fs, args); err != nil {
		return command.ParseExit(err)
	}
	id, err := resolveOrg(c, *org)
	if err != nil {
		return command.Fail(stderr, err)
	}
	return command.Call(c, "GET", orgPath(id, "/api-keys"), nil, "api_keys", "api-key list: none", stdout, stderr)
}

func ApiKeyRevoke(c *apiclient.Client, args []string, stdout, stderr io.Writer) int {
	fs := command.NewFlags("api-key revoke", "usage: hl api-key revoke ID\n\nEnds the key and every key minted from it, at once.", stderr)
	id, err := command.ParseOne(fs, args, "ID", "", "id")
	if err != nil {
		return command.ParseExit(err)
	}
	return command.Call(c, "DELETE", "/api-keys/"+url.PathEscape(id), nil, "", "revoked API key "+id, stdout, stderr)
}
