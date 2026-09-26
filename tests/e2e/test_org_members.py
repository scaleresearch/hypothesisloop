"""Org members, invites and what each role may do (plan 15.1).

A person exists once they sign in, so every person here signs in with a JWT of the local identity
provider and gets a home org on their first request. Everything else goes through the API and
`hl`: an owner invites by a token, a signed-in person accepts it, a member is held to a member's
limits, and an org never loses its last owner.
"""
from __future__ import annotations

import json
import uuid
from dataclasses import dataclass

import pytest
import requests

from support import signin
from support.api import API, HLError

pytestmark = pytest.mark.parallel


@dataclass
class Person:
    subject: str
    name: str
    # The person in their own home org.
    home: API

    def in_org(self, org_id: str, project_id: str) -> API:
        """The same sign-in, working in another org they belong to."""
        return self.home.as_caller(self.home.token, org_id, project_id)


def _person(api: API, run_id: str, label: str) -> Person:
    subject = f"members-{label}-{uuid.uuid4().hex[:10]}-{run_id}"
    name = f"{label.title()} {run_id}"
    token = signin.jwt(subject, name)
    me = requests.get(f"{api.base_url}/me", headers={"Authorization": f"Bearer {token}"}, timeout=30)
    assert me.status_code == 200, f"first request of {label}: {me.status_code} {me.text}"
    home = me.json()
    assert len(home["orgs"]) == 1 and home["orgs"][0]["role"] == "owner", f"a new person's home: {home}"
    assert home["orgs"][0]["name"] == "Default org" and home["projects"][0]["name"] == "Default project", f"a new person's home: {home}"
    assert home["name"] == name, f"a person is called what their sign-in states: {home}"
    return Person(subject, name, api.as_caller(token, home["orgs"][0]["id"], home["projects"][0]["id"]))


def _call(api: API, method: str, path: str, body: dict | None = None) -> requests.Response:
    return api.session.request(method, api._url(path), json=body, timeout=30)


def _refused(api: API, method: str, path: str, body: dict | None, status: int, reason: str) -> None:
    r = _call(api, method, path, body)
    assert r.status_code == status, f"{method} {path}: {r.status_code} {r.text}, want {status}"
    assert r.json().get("reason") == reason, f"{method} {path}: {r.text}, want reason {reason}"


def _invite(owner: API, role: str) -> dict:
    r = _call(owner, "POST", f"/orgs/{owner.org_id}/invites", {"role": role})
    assert r.status_code == 201, f"create invite: {r.status_code} {r.text}"
    invite = r.json()
    assert invite["id"].startswith("inv-") and invite["token"].startswith("hli_"), f"an invite and its token: {invite}"
    assert invite["org_id"] == owner.org_id and invite["role"] == role
    return invite


def _join(person: Person, owner: API, role: str) -> API:
    """Brings the person into the owner's org by an invite and returns them working there."""
    invite = _invite(owner, role)
    r = _call(person.home, "POST", "/invites/accept", {"token": invite["token"]})
    assert r.status_code == 201, f"accept: {r.status_code} {r.text}"
    assert r.json() == {**r.json(), "org_id": owner.org_id, "user_id": person.subject, "role": role}
    return person.in_org(owner.org_id, owner.project_id)


def _members(api: API, org_id: str) -> dict[str, str]:
    r = _call(api, "GET", f"/orgs/{org_id}/members")
    assert r.status_code == 200, f"list members: {r.status_code} {r.text}"
    assert int(r.headers["X-Total-Count"]) == len(r.json())
    return {m["user_id"]: m["role"] for m in r.json()}


def _lines(out: str) -> list[dict]:
    return [json.loads(line) for line in out.splitlines() if line.strip()]


def test_an_invite_brings_a_person_into_the_org_once(api: API, run_id: str):
    owner, guest, late = (_person(api, run_id, label) for label in ("owner", "guest", "late"))
    org = owner.home.org_id

    assert _call(guest.home, "GET", f"/orgs/{org}/members").status_code == 404, "an org is not found by someone outside it"
    created = json.loads(owner.home.hl("org", "invites", "create", "--role", "member"))
    assert created["id"].startswith("inv-") and created["token"].startswith("hli_") and created["actor"]["id"] == owner.subject

    listed = _call(owner.home, "GET", f"/orgs/{org}/invites")
    assert listed.status_code == 200 and listed.headers["X-Total-Count"] == "1"
    assert [i["id"] for i in listed.json()] == [created["id"]] and "token" not in listed.text, f"pending invites: {listed.text}"
    assert created["actor"] == {"kind": "user", "id": owner.subject, "name": owner.name} and listed.json()[0]["actor"] == created["actor"], "an invite names who made it"
    assert [i["id"] for i in _lines(owner.home.hl("org", "invites", "list"))] == [created["id"]]

    _refused(owner.home, "POST", "/invites/accept", {"token": created["token"]}, 409, "already_member")
    _refused(guest.home, "POST", "/invites/accept", {"token": "hli_" + uuid.uuid4().hex}, 404, "not_found")

    joined = json.loads(guest.home.hl("org", "invites", "accept", created["token"]))
    assert joined == {**joined, "org_id": org, "user_id": guest.subject, "role": "member"}
    _refused(late.home, "POST", "/invites/accept", {"token": created["token"]}, 409, "invite_used")
    _refused(owner.home, "DELETE", f"/invites/{created['id']}", None, 409, "invite_used")
    assert _call(owner.home, "GET", f"/orgs/{org}/invites").json() == [], "an accepted invite is no longer pending"

    assert _members(owner.home, org) == {owner.subject: "owner", guest.subject: "member"}
    listed_members = _lines(owner.home.hl("org", "members", "list"))
    assert {m["user_id"]: m["name"] for m in listed_members} == {owner.subject: owner.name, guest.subject: guest.name}, \
        "each member is listed under their name, to every member"
    assert {m["user_id"]: m["name"] for m in _call(guest.in_org(org, owner.home.project_id), "GET", f"/orgs/{org}/members").json()} == {
        owner.subject: owner.name, guest.subject: guest.name}
    roles = {o["id"]: o["role"] for o in guest.home.get_json("/me")["orgs"]}
    assert roles == {guest.home.org_id: "owner", org: "member"}, f"the orgs of the one who joined: {roles}"
    assert late.subject not in _members(owner.home, org)


def test_an_invite_that_was_revoked_admits_nobody(api: API, run_id: str):
    owner, guest, stranger = (_person(api, run_id, label) for label in ("owner", "guest", "stranger"))
    invite = _invite(owner.home, "owner")

    assert _call(stranger.home, "DELETE", f"/invites/{invite['id']}").status_code == 404, "another org's invite is not found"
    assert _call(stranger.home, "DELETE", f"/invites/inv-{uuid.uuid4().hex}").status_code == 404
    owner.home.hl("org", "invites", "revoke", invite["id"])
    assert _call(owner.home, "DELETE", f"/invites/{invite['id']}").status_code == 204, "revoking a revoked invite changes nothing"
    _refused(guest.home, "POST", "/invites/accept", {"token": invite["token"]}, 409, "invite_revoked")
    assert guest.subject not in _members(owner.home, owner.home.org_id)

    _refused(owner.home, "POST", f"/orgs/{owner.home.org_id}/invites", {"role": "admin"}, 422, "invalid_role")
    _refused(owner.home, "GET", f"/orgs/{owner.home.org_id}/invites?limit=201", None, 422, "invalid_page")


def test_a_member_reads_the_org_and_changes_none_of_what_an_owner_holds(api: API, run_id: str):
    owner, guest = _person(api, run_id, "owner"), _person(api, run_id, "guest")
    org, project = owner.home.org_id, owner.home.project_id
    owners_key = owner.home.post_json(f"/orgs/{org}/api-keys", {"name": "the owner's"})
    member = _join(guest, owner.home, "member")

    for path in (f"/orgs/{org}/projects", f"/orgs/{org}/api-keys", f"/orgs/{org}/clusters", f"/orgs/{org}/members"):
        assert _call(member, "GET", path).status_code == 200, f"a member reads {path}"
    for method, path, body in [
        ("PATCH", f"/orgs/{org}", {"name": "taken over"}),
        ("POST", f"/orgs/{org}/projects", {"name": "escape"}),
        ("PATCH", f"/projects/{project}", {"name": "escape"}),
        ("POST", f"/orgs/{org}/api-keys", {"name": "org-wide"}),
        ("DELETE", f"/api-keys/{owners_key['id']}", None),
        ("POST", f"/orgs/{org}/clusters", {"name": f"escape-{uuid.uuid4().hex[:8]}"}),
        ("POST", f"/orgs/{org}/invites", {"role": "member"}),
        ("GET", f"/orgs/{org}/invites", None),
        ("PATCH", f"/orgs/{org}/members/{guest.subject}", {"role": "owner"}),
        ("PATCH", f"/orgs/{org}/members/{owner.subject}", {"role": "member"}),
        ("DELETE", f"/orgs/{org}/members/{owner.subject}", None),
    ]:
        r = _call(member, method, path, body)
        assert r.status_code == 403, f"a member's {method} {path}: {r.status_code} {r.text}, want 403"
    with pytest.raises(HLError, match="403"):
        member.hl("org", "invites", "create")
    assert _members(owner.home, org) == {owner.subject: "owner", guest.subject: "member"}

    # What a member is in the org for: platform experiments and what goes on in them.
    pe_id = member.create_platform_experiment(f"members-{uuid.uuid4().hex[:8]}-{run_id}", 1.0, 1)
    try:
        assert member.get_json(f"/platform-experiments/{pe_id}")["project_id"] == project
        assert owner.home.get_json(f"/platform-experiments/{pe_id}")["id"] == pe_id
    finally:
        member.close_platform_experiment(pe_id)

    # A member's own keys are bound to one project, and a key goes when its creator does.
    created = _call(member, "POST", f"/orgs/{org}/api-keys", {"name": "a member's", "project_id": project})
    assert created.status_code == 201, f"a member's project key: {created.status_code} {created.text}"
    key = created.json()
    assert key["actor"]["id"] == guest.subject and key["project_id"] == project
    with_key = api.as_caller(key["token"], org, project)
    assert with_key.get_json("/me")["kind"] == "key"
    second = _call(member, "POST", f"/orgs/{org}/api-keys", {"name": "another", "project_id": project}).json()
    assert _call(member, "DELETE", f"/api-keys/{second['id']}").status_code == 200, "a member revokes a key they made"

    assert _call(member, "DELETE", f"/orgs/{org}/members/{guest.subject}").status_code == 204, "a member leaves by themselves"
    assert with_key.get("/me").status_code == 401, "the key of someone who left the org still works"
    assert _call(member, "GET", f"/orgs/{org}/members").status_code == 404, "the org is still found by the one who left it"
    assert [o["id"] for o in guest.home.get_json("/me")["orgs"]] == [guest.home.org_id]
    assert _members(owner.home, org) == {owner.subject: "owner"}


def test_an_org_keeps_its_last_owner(api: API, run_id: str):
    first, second = _person(api, run_id, "first"), _person(api, run_id, "second")
    org = first.home.org_id
    me = f"/orgs/{org}/members/{first.subject}"

    _refused(first.home, "DELETE", me, None, 409, "last_owner")
    _refused(first.home, "PATCH", me, {"role": "member"}, 409, "last_owner")
    with pytest.raises(HLError, match="last_owner"):
        first.home.hl("org", "members", "remove", first.subject)
    assert _members(first.home, org) == {first.subject: "owner"}

    other = _join(second, first.home, "owner")
    wide = first.home.post_json(f"/orgs/{org}/api-keys", {"name": "org-wide"})
    with_wide = api.as_caller(wide["token"], org, first.home.project_id)
    assert with_wide.get("/me").status_code == 200
    pending = _invite(first.home, "owner")

    # With a second owner the first may step down, and what only an owner holds goes with it.
    demoted = json.loads(other.hl("org", "members", "set-role", first.subject, "member"))
    assert demoted == {**demoted, "user_id": first.subject, "role": "member"}
    assert with_wide.get("/me").status_code == 401, "an org-wide key outlived its creator's ownership"
    third = _person(api, run_id, "third")
    _refused(third.home, "POST", "/invites/accept", {"token": pending["token"]}, 409, "invite_revoked")
    assert _call(first.home, "POST", f"/orgs/{org}/invites", {"role": "owner"}).status_code == 403

    _refused(other, "DELETE", f"/orgs/{org}/members/{second.subject}", None, 409, "last_owner")
    _refused(other, "PATCH", f"/orgs/{org}/members/{second.subject}", {"role": "member"}, 409, "last_owner")
    _refused(other, "DELETE", f"/orgs/{org}/members/{third.subject}", None, 404, "not_found")
    assert _call(other, "DELETE", me).status_code == 204
    assert _members(other, org) == {second.subject: "owner"}
    assert _call(first.home, "GET", f"/orgs/{org}/members").status_code == 404


def test_an_api_key_manages_members_and_joins_nothing(api: API, run_id: str):
    owner, guest = _person(api, run_id, "owner"), _person(api, run_id, "guest")
    org = owner.home.org_id
    key = owner.home.post_json(f"/orgs/{org}/api-keys", {"name": "ci"})
    with_key = api.as_caller(key["token"], org, owner.home.project_id)

    invite = json.loads(with_key.hl("org", "invites", "create"))
    assert invite["role"] == "member" and invite["actor"]["id"] == owner.subject, "an invite made with a key is answered for by the key's creator"
    refused = _call(with_key, "POST", "/invites/accept", {"token": invite["token"]})
    assert refused.status_code == 403, f"an API key accepting an invite: {refused.status_code} {refused.text}"
    assert _call(guest.home, "POST", "/invites/accept", {"token": invite["token"]}).status_code == 201

    promoted = json.loads(with_key.hl("org", "members", "set-role", guest.subject, "owner"))
    assert promoted["role"] == "owner"
    with_key.hl("org", "members", "remove", guest.subject)
    assert _members(with_key, org) == {owner.subject: "owner"}


def test_an_account_creates_up_to_its_limits(api: API, run_id: str):
    """Every person of the suite is a distinct one, so each starts with the one org of their first
    sign-in and the one project it came with. The bounds are read from /me, not assumed."""
    person = _person(api, run_id, "limits")
    me = person.home.get_json("/me")
    max_orgs, max_projects = me["limits"]["max_orgs_per_user"], me["limits"]["max_projects_per_org"]
    assert max_orgs >= 2 and max_projects >= 2, f"limits of the deployment: {me['limits']}"

    for i in range(1, max_orgs):
        created = _call(person.home, "POST", "/orgs", {"name": f"org {i} {run_id}"})
        assert created.status_code == 201, f"org {i + 1} of {max_orgs}: {created.status_code} {created.text}"
    _refused(person.home, "POST", "/orgs", {"name": f"one more {run_id}"}, 409, "max_orgs_reached")
    with pytest.raises(HLError, match="max_orgs_reached"):
        person.home.hl("org", "create", f"one more {run_id}")
    owned = [o for o in person.home.get_json("/me")["orgs"] if o["role"] == "owner"]
    assert len(owned) == max_orgs, f"orgs owned after the refusal: {owned}"

    org = person.home.org_id
    for i in range(1, max_projects):
        created = _call(person.home, "POST", f"/orgs/{org}/projects", {"name": f"project-{i}"})
        assert created.status_code == 201, f"project {i + 1} of {max_projects}: {created.status_code} {created.text}"
    _refused(person.home, "POST", f"/orgs/{org}/projects", {"name": "one-more"}, 409, "max_projects_reached")
    with pytest.raises(HLError, match="max_projects_reached"):
        person.home.hl("project", "create", "one-more")
    held = [p for p in person.home.get_json("/me")["projects"] if p["org_id"] == org]
    assert len(held) == max_projects, f"projects of the org after the refusal: {held}"

    # Staff are held to neither bound, whoever they create for.
    assert "limits" not in api.get_json("/me")
    by_staff = api.post("/orgs", {"name": f"past the limit {run_id}", "owner_user_id": person.subject})
    assert by_staff.status_code == 201, f"staff creating past a person's limit: {by_staff.status_code} {by_staff.text}"
    assert api.post(f"/orgs/{org}/projects", {"name": "past-the-limit"}).status_code == 201
