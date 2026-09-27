"""One typed-enough API client (tests/improve.md #2.4): requests for HTTP, subprocess for `hl` —
the CLI path a real agent uses for registration/signup/submission. Every call returns a dict (or a
small dataclass); assertions live in the test, not buried in a shell string comparison.

Fetch ONE state snapshot per assertion group: `experiment()`/`platform_experiment()` return the
full body so a test can assert on several fields without risking two different moments.
"""
from __future__ import annotations

import base64
import json
import os
import subprocess
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import requests
import yaml

from . import admin

REPO_ROOT = Path(__file__).resolve().parents[2]
JOB_SPEC_PATH = REPO_ROOT / "tests" / "workloads" / "generic" / "job.yaml"


def workload_image() -> str:
    """The generic workload as cluster nodes pull it: from the local registry at the host address
    `make render-settings` wrote (nodes cannot reach the host's localhost), under the tag
    `make images` last pushed. Never latest: nodes pull IfNotPresent and keep a stale latest."""
    host = (REPO_ROOT / "localdev" / ".host-address").read_text().strip()
    tag = (REPO_ROOT / "localdev" / ".image-tag").read_text().strip()
    return f"{host}:5000/hypothesisloop-workload:{tag}"


def link(type_: str, to: str) -> dict:
    """One entry of a POST /hypotheses `links` list; `to` is a hypothesis id or the platform
    experiment id for the objective root."""
    return {"type": type_, "to": to}


def token_scope(token: str) -> tuple[str, str, str]:
    """What a token the platform derived says of its holder: (kind, platform experiment, agent),
    the agent empty for a coordinator. A launched machine is told who it is by its token alone."""
    part = token.removeprefix("hls_").split(".", 1)[0]
    kind, _, rest = base64.urlsafe_b64decode(part + "=" * (-len(part) % 4)).decode().partition(":")
    pe, _, agent = rest.partition("/")
    return kind, pe, agent


@dataclass
class HLError(RuntimeError):
    cmd: list[str]
    returncode: int
    output: str

    def __str__(self) -> str:  # pragma: no cover - trivial
        return f"`{' '.join(self.cmd)}` exited {self.returncode}: {self.output}"


@dataclass
class JobRequest:
    """A POST /experiments body, mirroring tests/workloads/generic/job.yaml's shape: a full
    {id, metadata, job} document. `job` is loaded from JOB_SPEC_PATH once and overridden per call
    -- dict -> yaml.safe_dump, never envsubst/.tmpl (tests/improve.md #2.4)."""

    id: str
    agent_id: str
    platform_experiment_id: str
    hypothesis_id: str
    code_ref: str
    hours: float
    tier: str = "guaranteed"
    job_overrides: dict[str, Any] = field(default_factory=dict)
    metadata_overrides: dict[str, Any] = field(default_factory=dict)

    def to_yaml(self) -> str:
        job = yaml.safe_load(JOB_SPEC_PATH.read_text())["job"]
        job["image"] = workload_image()
        for k, v in self.job_overrides.items():
            if v is None:
                job.pop(k, None)
            else:
                job[k] = v
        # Mirrors tests/lib/mk_body.py: an explicit accelerator_type override also drops
        # acceptable_accelerator_types, so a scenario asking for a specific (possibly
        # unsatisfiable) type is never silently satisfied by a cheaper alternate -- unless the
        # caller already named acceptable_accelerator_types itself, which wins either way.
        if "accelerator_type" in self.job_overrides and "acceptable_accelerator_types" not in self.job_overrides:
            job.pop("acceptable_accelerator_types", None)
        metadata = {
            "agent_id": self.agent_id,
            "platform_experiment_id": self.platform_experiment_id,
            "hypothesis_id": self.hypothesis_id,
            "theory": "e2e pytest coverage",
            "code_ref": self.code_ref,
            "config_hash": "",
            "data_ref": "",
            "capacity_tier": self.tier,
            "estimated_duration_hours": self.hours,
        }
        for k, v in self.metadata_overrides.items():
            if v is None:
                metadata.pop(k, None)
            else:
                metadata[k] = v
        body = {"id": self.id, "metadata": metadata, "job": job}
        return yaml.safe_dump(body, sort_keys=False)


class API:
    def __init__(self, base_url: str, hl_bin: Path, token: str, org_id: str, project_id: str):
        """One caller: every request and every `hl` run carries `token`. `org_id` is where its
        agents register and `project_id` where its platform experiments are created."""
        self.base_url = base_url.rstrip("/")
        self.hl_bin = hl_bin
        self.token = token
        self.org_id = org_id
        self.project_id = project_id
        # The repository its platform experiments name unless a create says otherwise.
        self.repository: str | None = None
        self.session = requests.Session()
        self.session.headers["Authorization"] = f"Bearer {token}"
        # Every platform experiment created through hl by this caller and the callers made from
        # it, closed by conftest at session end.
        self.created_platform_experiments: list[str] = []

    # ---- hl CLI -----------------------------------------------------------------------------

    def hl(self, *args: str, input: str | None = None, env: dict[str, str] | None = None) -> str:
        cmd = [str(self.hl_bin), *args]
        proc = subprocess.run(
            cmd,
            input=input,
            capture_output=True,
            text=True,
            env={**self.hl_env(), **(env or {})},
            timeout=60,
        )
        if proc.returncode != 0:
            raise HLError(cmd, proc.returncode, proc.stdout + proc.stderr)
        if args[:2] == ("platform-experiments", "create"):
            self.created_platform_experiments.append(json.loads(proc.stdout)["id"])
        return proc.stdout

    # Whether the stack keeps its code on GitHub, the orgs the sandbox installation is already
    # connected to, and on any other stack the repository made for the session.
    _github_app: bool | None = None
    _connected: set[str] = set()
    local_repository: Path | None = None

    def sandbox_repository(self) -> str:
        """The repository a create names when the test names none. On a stack whose git host is
        GitHub it is the sandbox one, since a create that names none would have the platform
        create a new repository on github.com each time, and the installation that covers it is
        connected to this caller's org first. On any other stack it is a bare repository made
        once for the session in the directory the local git server serves (HL_E2E_GIT_ROOT)."""
        if API._github_app is None:
            API._github_app = self.get("/github/app").status_code != 503
        if not API._github_app:
            if API.local_repository is None:
                root = os.environ.get("HL_E2E_GIT_ROOT")
                if not root:
                    raise RuntimeError(
                        "this stack keeps its code on its own git server, where the platform creates no repository: "
                        "set HL_E2E_GIT_ROOT to the directory that server serves (make e2e does)")
                created = Path(root) / f"e2e-{uuid.uuid4().hex[:8]}.git"
                subprocess.run(["git", "init", "-q", "--bare", str(created)], check=True)
                API.local_repository = created
            return API.local_repository.name
        installation, repository = os.environ.get("HL_E2E_GITHUB_INSTALLATION"), os.environ.get("HL_E2E_GITHUB_REPO")
        if not installation or not repository:
            raise RuntimeError(
                "this stack has a GitHub App, so every platform experiment names a repository: "
                "set HL_E2E_GITHUB_INSTALLATION and HL_E2E_GITHUB_REPO (.env.example says what each is)")
        if self.org_id and self.org_id not in API._connected:
            listed = self.get_json(f"/orgs/{self.org_id}/github-installations", params={"limit": 200})
            if not any(i["installation_id"] == int(installation) for i in listed):
                admin.connect_github(self.org_id, installation)
            API._connected.add(self.org_id)
        return repository

    def hl_env(self) -> dict[str, str]:
        """The env `hl()`/`hl_expect()` invoke the CLI with -- exposed so a test that needs its own
        subprocess.Popen (e.g. a long-running `hl watch` run concurrently with another action) can
        build an identical child process."""
        return {
            "API_URL": self.base_url, "HL_TOKEN": self.token, "HL_ORG": self.org_id, "HL_PROJECT": self.project_id,
            "PATH": "/usr/bin:/bin:/usr/local/bin",
        }

    def as_caller(self, token: str, org_id: str = "", project_id: str = "") -> "API":
        """The same API seen by another caller. What it creates is listed with what this one
        creates, so the session closes it too."""
        caller = API(self.base_url, self.hl_bin, token, org_id, project_id)
        caller.created_platform_experiments = self.created_platform_experiments
        return caller

    def hl_expect(self, *args: str, input: str | None = None, timeout: float = 60) -> subprocess.CompletedProcess:
        """Raw `hl` invocation returning the CompletedProcess as-is, unlike hl() which raises on a
        non-zero exit -- for tests asserting the CLI's own exit code and stderr text (usage errors,
        CLI-surfaced API failures), not just successful JSON output."""
        cmd = [str(self.hl_bin), *args]
        return subprocess.run(cmd, input=input, capture_output=True, text=True, env=self.hl_env(), timeout=timeout)

    # ---- plain HTTP helpers ------------------------------------------------------------------

    def _url(self, path: str) -> str:
        return f"{self.base_url}{path}"

    def get(self, path: str, **kw) -> requests.Response:
        return self.session.get(self._url(path), timeout=30, **kw)

    def post(self, path: str, json_body: dict | None = None, **kw) -> requests.Response:
        if path in ("/platform-experiments", "/platform-experiments/drafts") and json_body is not None and "repository" not in json_body:
            json_body = {**json_body, "repository": self.repository or self.sandbox_repository()}
        return self.session.post(self._url(path), json=json_body, timeout=30, **kw)

    def put(self, path: str, json_body: dict | None = None, **kw) -> requests.Response:
        return self.session.put(self._url(path), json=json_body, timeout=30, **kw)

    def patch(self, path: str, json_body: dict | None = None, **kw) -> requests.Response:
        return self.session.patch(self._url(path), json=json_body, timeout=30, **kw)

    def get_json(self, path: str, **kw) -> Any:
        r = self.get(path, **kw)
        r.raise_for_status()
        return r.json()

    def post_json(self, path: str, json_body: dict | None = None, **kw) -> Any:
        r = self.post(path, json_body, **kw)
        r.raise_for_status()
        return r.json()

    # ---- agents -------------------------------------------------------------------------------

    def register_agent(self, agent_id: str) -> None:
        body = {"id": agent_id, "name": agent_id, **({"org_id": self.org_id} if self.org_id else {})}
        r = self.post("/agents", body)
        assert r.status_code == 201, f"register agent {agent_id}: {r.status_code} {r.text}"

    # ---- platform experiments -------------------------------------------------------------------

    def code_ref(self, pe_id: str) -> str:
        """What a job of the platform experiment names as its code: the experiment's repository
        at a commit. Admission checks the address. The suite's workload is in its image and
        clones nothing, so the commit is one no repository holds."""
        return self.get_json(f"/platform-experiments/{pe_id}")["repository_url"] + "@" + "a" * 40

    def create_platform_experiment(
        self,
        name: str,
        budget_accelerator_hours: float,
        max_agents: int,
        report_interval_seconds: int = 10,
        metrics: list[dict] | None = None,
        stages: list[dict] | None = None,
        max_concurrent_accelerators: int | None = None,
        repository: str | None = None,
    ) -> str:
        body: dict[str, Any] = {
            "name": name,
            "description": name,
            "budget_accelerator_hours": budget_accelerator_hours,
            "max_agents": max_agents,
            "report_interval_seconds": report_interval_seconds,
            "metrics": metrics or [{"key": "val_accuracy", "direction": "maximize"}],
            # No starts_at: it starts when a test starts it. The end is far enough out that no
            # test outlives it.
            "ends_at": (datetime.now(timezone.utc) + timedelta(days=7)).strftime("%Y-%m-%dT%H:%M:%SZ"),
        }
        if stages is not None:
            body["stages"] = stages
        if max_concurrent_accelerators is not None:
            body["max_concurrent_accelerators"] = max_concurrent_accelerators
        body["repository"] = repository or self.repository or self.sandbox_repository()
        out = self.hl("platform-experiments", "create", "-", input=yaml.safe_dump(body, sort_keys=False))
        return json.loads(out)["id"]

    # ---- clusters (autoscaler.md) -----------------------------------------------------------

    def list_internal_clusters(self) -> list[dict]:
        """`GET /internal/clusters` -- id/name/autoscaler_enabled for every cluster the control
        plane has heard from, per autoscaler.md's cluster-identity design."""
        return self.get_json("/internal/clusters")["clusters"]

    def cluster_id_for_name(self, cluster_name: str) -> str | None:
        for c in self.list_internal_clusters():
            if c["cluster_name"] == cluster_name:
                return c["cluster_id"] or None
        return None

    def set_cluster_settings(self, cluster_name: str, *, org_id: str = "default", **settings: int | None) -> None:
        """Changes the settings named on a cluster of org_id; None puts one back on the scheduler's
        default, and naming none puts all three back."""
        names = settings or dict.fromkeys(
            ("scale_up_timeout_seconds", "max_speculative_accelerators", "stuck_pending_timeout_seconds"))
        body = {name: settings.get(name) or 0 for name in names}
        self.patch(f"/orgs/{org_id}/clusters/{cluster_name}", body).raise_for_status()

    def signup(self, pe_id: str, agent_id: str, role: str | None = None, quota_tier: str | None = "guaranteed") -> requests.Response:
        # Default "guaranteed", not the server's own kind-based default: an e2e-registered agent
        # is AgentKindAgent, which the platform resolves to burst_only (zero guaranteed quota)
        # unless overridden -- see domain.ResolveQuotaTier. Every scenario ported from the
        # original bash suite assumes unconditional guaranteed-tier eligibility (that predates the
        # human/agent kind distinction), so the harness opts every signup into the normal
        # guaranteed+burst split by default; QuotaTierGuaranteed still carries the full burst
        # allocation too, so this never blocks a test that submits burst-tier jobs. Pass
        # quota_tier="burst_only" (or via signup_and_start's 3-tuple) explicitly for a test that
        # means to exercise the kind-based default itself.
        payload: dict[str, Any] = {"agent_id": agent_id}
        if role:
            payload["role"] = role
        if quota_tier:
            payload["quota_tier"] = quota_tier
        return self.post(f"/platform-experiments/{pe_id}/agents", payload)

    def signup_ok(self, pe_id: str, agent_id: str, role: str | None = None, quota_tier: str | None = "guaranteed") -> None:
        r = self.signup(pe_id, agent_id, role, quota_tier)
        r.raise_for_status()

    def add_agents(self, pe_id: str, count: int = 1, **body: Any) -> list[dict]:
        """POST /platform-experiments/{id}/agents with no agent_id, `count` times: each call has
        the platform create one agent. Returns each one as answered, with its token when it is
        not hosted."""
        added: list[dict] = []
        for _ in range(count):
            answered = self.post_json(f"/platform-experiments/{pe_id}/agents", body)["agents"]
            assert len(answered) == 1, f"one call added {len(answered)} agents: {answered}"
            added += answered
        return added

    def agents(self, pe_id: str) -> list[dict]:
        return self.get_json(f"/platform-experiments/{pe_id}/agents")["agents"]

    def remove_agent(self, pe_id: str, agent_id: str) -> requests.Response:
        return self.session.delete(self._url(f"/platform-experiments/{pe_id}/agents/{agent_id}"), timeout=30)

    # What a launcher says of its own launch inputs. Tests that ask as one launcher all say the
    # same, since a change of it renames every instance of that launcher.
    LAUNCH_DIGEST = "e2e0e2e0e2e0e2e0e2e0e2e0e2e0e2e0"

    def launcher_desired(self, launcher: str, observed: list[dict] | None = None) -> dict:
        """One reconcile call as the agent-launcher of that name makes it: `observed` is what its
        provider lists ({name, state, log}), and the answer is what should exist right now."""
        return self.post_json(f"/internal/launchers/{launcher}/reconcile", self.launcher_question(observed))

    def launcher_question(self, observed: list[dict] | None = None) -> dict:
        """What a launcher sends: what it observed, its launch digest and how it paces its passes."""
        return {"launch_digest": self.LAUNCH_DIGEST, "instances": observed or [],
                "reconcile_interval_seconds": 2, "create_pause_milliseconds": 0}

    def signup_role(self, pe_id: str, agent_id: str) -> tuple[int, str | None]:
        r = self.get(f"/platform-experiments/{pe_id}/signups/{agent_id}")
        if r.status_code == 404:
            return 404, None
        r.raise_for_status()
        return r.status_code, r.json().get("role")

    def start_platform_experiment(self, pe_id: str) -> None:
        self.hl("platform-experiments", "start", "--platform-experiment", pe_id)

    def signup_and_start(self, pe_id: str, agents: list[str] | list[tuple]) -> None:
        for a in agents:
            if isinstance(a, tuple):
                agent_id, role = a[0], a[1]
                quota_tier = a[2] if len(a) > 2 else "guaranteed"
                self.signup_ok(pe_id, agent_id, role, quota_tier)
            else:
                self.signup_ok(pe_id, a)
        self.start_platform_experiment(pe_id)

    def close_platform_experiment(self, pe_id: str) -> None:
        # The status is not checked: teardown also closes what the test body already closed.
        self.post(f"/platform-experiments/{pe_id}/close", {"top_results": []})

    def platform_experiment_quotas(self, pe_id: str) -> list[dict]:
        return self.get_json(f"/platform-experiments/{pe_id}/quotas")

    def quota_field(self, pe_id: str, agent_id: str, field_name: str) -> float:
        for q in self.platform_experiment_quotas(pe_id):
            if q["agent_id"] == agent_id:
                return q[field_name]
        raise LookupError(f"agent {agent_id} has no quota in {pe_id}")

    def stages(self, pe_id: str) -> dict:
        return self.get_json(f"/platform-experiments/{pe_id}/stages")

    def results(self, pe_id: str) -> dict:
        return self.get_json(f"/platform-experiments/{pe_id}/results")

    # ---- hypotheses ----------------------------------------------------------------------------

    def post_hypothesis(
        self, pe_id: str, text: str = "", agent_id: str = "", links: list[dict] | None = None,
        distinct_from: list[str] | None = None,
    ) -> requests.Response:
        """A hypothesis of agent_id's, or with none one no agent owns. Who registered it is the
        caller either way. links=None places the hypothesis on the objective root; pass [] to
        send none. The default text is one word per owner, so two owners' defaults never read alike."""
        return self.post(
            "/hypotheses",
            {
                "platform_experiment_id": pe_id,
                "text": text or f"e2e_{(agent_id or 'unowned').replace('-', '_')}",
                "links": [link("builds_on", pe_id)] if links is None else links,
                "distinct_from": distinct_from or [],
            } | ({"agent_id": agent_id} if agent_id else {}),
        )

    def register_hypothesis(
        self, pe_id: str, agent_id: str, text: str = "", links: list[dict] | None = None, distinct_from: list[str] | None = None
    ) -> dict:
        r = self.post_hypothesis(pe_id, text, agent_id=agent_id, links=links, distinct_from=distinct_from)
        r.raise_for_status()
        return r.json()

    def register_human_hypothesis(self, pe_id: str, text: str, links: list[dict] | None = None) -> dict:
        """A hypothesis no agent owns, made by whoever the token is: no agent_id."""
        r = self.post_hypothesis(pe_id, text, links=links)
        r.raise_for_status()
        return r.json()

    def hypothesis(self, hyp_id: str) -> dict:
        return self.get_json(f"/hypotheses/{hyp_id}")

    def hypotheses(self, pe_id: str, limit: int = 200, **filters: str) -> list[dict]:
        return self.get_json("/hypotheses", params={"platform_experiment_id": pe_id, "limit": limit, **filters})

    def post_hypothesis_link(self, hyp_id: str, type_: str, to: str, agent_id: str = "") -> requests.Response:
        return self.post(f"/hypotheses/{hyp_id}/links", {"type": type_, "to": to} | ({"agent_id": agent_id} if agent_id else {}))

    def hypothesis_graph(self, pe_id: str) -> dict:
        return self.get_json("/hypotheses/graph", params={"platform_experiment_id": pe_id})

    def query(self, pe_id: str, cypher: str, /, **params: Any) -> requests.Response:
        return self.post("/hypotheses/query", {"platform_experiment_id": pe_id, "cypher": cypher, "params": params})

    def query_rows(self, pe_id: str, cypher: str, /, **params: Any) -> list[dict]:
        """Rows of a POST /hypotheses/query as dicts keyed by column; fails on a truncated result
        so no assertion silently reads a partial answer."""
        r = self.query(pe_id, cypher, **params)
        assert r.status_code == 200, f"query refused ({r.status_code}): {r.text}"
        body = r.json()
        assert not body["truncated"], f"query result truncated: {cypher}"
        return [dict(zip(body["columns"], row, strict=True)) for row in body["rows"]]

    def post_hypothesis_comment(self, hyp_id: str, text: str, agent_id: str = "") -> requests.Response:
        """A comment of agent_id's, or with none the caller's own. Who wrote it is the caller either way."""
        return self.post(f"/hypotheses/{hyp_id}/comments", {"text": text} | ({"agent_id": agent_id} if agent_id else {}))

    # What a test says for a confirmed or refuted verdict it gives without the jobs that carry one.
    NO_EVIDENCE = "the test settles this hypothesis without running jobs for it"

    def set_hypothesis_status(self, hyp_id: str, agent_id: str, status: str, **evidence: str) -> requests.Response:
        """Sets a verdict. confirmed and refuted need `against` (the control job) or `despite`
        (a reason); with neither given the test's own reason is sent."""
        if status in ("confirmed", "refuted") and not evidence:
            evidence = {"despite": self.NO_EVIDENCE}
        return self.post(f"/hypotheses/{hyp_id}/status", {"status": status, **evidence} | ({"agent_id": agent_id} if agent_id else {}))

    # ---- jobs / experiments ---------------------------------------------------------------------

    def _job_id(self) -> str:
        return f"job-{uuid.uuid4().hex[:8]}"

    def submit_job(
        self,
        pe_id: str,
        agent_id: str,
        hours: float = 0.02,
        tier: str = "guaranteed",
        job_overrides: dict[str, Any] | None = None,
        metadata_overrides: dict[str, Any] | None = None,
        hyp_text: str = "",
        hyp_id: str | None = None,
        job_id: str | None = None,
        secret_env: dict[str, str] | None = None,
    ) -> str:
        """Registers a hypothesis for the agent (unless hyp_id names an existing row) and submits
        the job through the `hl` CLI -- the real submission path an agent uses. secret_env reaches
        `hl` the only way it takes one: in its environment, named by --secret-env."""
        if hyp_id is None:
            hyp_id = self.register_hypothesis(pe_id, agent_id, hyp_text)["id"]
        jid = job_id or self._job_id()
        req = JobRequest(
            id=jid,
            agent_id=agent_id,
            platform_experiment_id=pe_id,
            hypothesis_id=hyp_id,
            code_ref=self.code_ref(pe_id),
            hours=hours,
            tier=tier,
            job_overrides=job_overrides or {},
            metadata_overrides=metadata_overrides or {},
        )
        secret_flags = [arg for name in (secret_env or {}) for arg in ("--secret-env", name)]
        self.hl("job", "submit", "--agent", agent_id, *secret_flags, "-", input=req.to_yaml(), env=secret_env)
        return jid

    def submit_job_expect(
        self,
        pe_id: str,
        agent_id: str,
        hours: float = 0.02,
        tier: str = "guaranteed",
        job_overrides: dict[str, Any] | None = None,
        hyp_id: str | None = None,
    ) -> tuple[int, str]:
        """Raw HTTP submission (not through `hl`) so the caller can see the exact admission-time
        status code, including a refusal that never persists anything."""
        if hyp_id is None:
            hyp_id = self.register_hypothesis(pe_id, agent_id, "")["id"]
        jid = self._job_id()
        req = JobRequest(
            id=jid,
            agent_id=agent_id,
            platform_experiment_id=pe_id,
            hypothesis_id=hyp_id,
            code_ref=self.code_ref(pe_id),
            hours=hours,
            tier=tier,
            job_overrides=job_overrides or {},
        )
        body = yaml.safe_load(req.to_yaml())
        r = self.post("/experiments", body)
        return r.status_code, jid

    def submission_body_for_id(self, job_id: str, pe_id: str, agent_id: str, tier: str = "guaranteed", hours: float = 0.02) -> dict:
        hyp_id = self.register_hypothesis(pe_id, agent_id, "")["id"]
        req = JobRequest(id=job_id, agent_id=agent_id, platform_experiment_id=pe_id, hypothesis_id=hyp_id, code_ref=self.code_ref(pe_id), hours=hours, tier=tier)
        return yaml.safe_load(req.to_yaml())

    def post_experiment_body(self, body: dict) -> requests.Response:
        return self.post("/experiments", body)

    def experiment(self, job_id: str) -> dict:
        """One snapshot of an experiment's full state (tests/improve.md #2.4: fetch once per
        assertion group, never one HTTP call per field)."""
        r = self.get(f"/experiments/{job_id}")
        r.raise_for_status()
        return r.json()

    def cancel_job(self, job_id: str) -> None:
        self.post(f"/experiments/{job_id}/cancel").raise_for_status()

    def admit(self, job_id: str, cluster_name: str) -> requests.Response:
        """POST /experiments/{id}/admit: force-admits a QUEUED experiment onto a named cluster,
        going through the same capacity-claiming transition ordinary admission uses (see
        controlplane/services/scheduler/handler.go's admit) -- for tests that must pin a job to a
        specific cluster rather than leaving cluster choice to the scheduler."""
        return self.post(f"/experiments/{job_id}/admit", {"cluster_name": cluster_name})

    def experiment_data(self, job_id: str) -> requests.Response:
        """Raw response for GET /experiments/{id}/data (a live listing of what the job left under
        its own prefix), so a caller can distinguish HTTP status from an empty-but-200 body -- a
        job that wrote nothing lists as an empty array, not an error (tests/improve.md #4: port
        assertions 1:1, including the ones that check this distinction)."""
        return self.get(f"/experiments/{job_id}/data")

    def lineage(self, job_id: str) -> list[dict]:
        """GET /experiments/{id}/lineage: the chain this experiment was derived from, oldest
        first. A job with no parent is a chain of one (itself), not an empty list."""
        return self.get_json(f"/experiments/{job_id}/lineage")

    def file_finding(self, job_id: str, summary: str | None = None) -> None:
        self.post(
            f"/experiments/{job_id}/summary", {"summary": summary or f"e2e pytest finding for {job_id}"}
        ).raise_for_status()

    def needs_summary(self, agent_id: str, pe_id: str, limit: int = 50) -> list[str]:
        docs = self.get_json(
            f"/experiments?needs_summary=true&agent_id={agent_id}&platform_experiment_id={pe_id}&limit={limit}"
        )
        return [e["id"] for e in docs]

    def logs(self, job_id: str, n: int = 200) -> str:
        r = self.get(f"/experiments/{job_id}/logs", params={"n": n})
        r.raise_for_status()
        return r.text

    def metrics(self, job_id: str) -> list[dict]:
        return self.get_json(f"/experiments/{job_id}/metrics")

    def metric_values(self, job_id: str, metric_name: str) -> list[float]:
        return [p["metric_value"] for p in self.metrics(job_id) if p["metric_name"] == metric_name]

    def metric_max(self, job_id: str, metric_name: str) -> float | None:
        values = self.metric_values(job_id, metric_name)
        return max(values) if values else None

    def post_metric(
        self, job_id: str, fraction_complete: float, metric_value: float, metric_name: str = "val_accuracy"
    ) -> requests.Response:
        """Raw metric ingestion, for asserting on the HTTP status of a malformed/unknown-target
        sample (never raises on 4xx -- the caller inspects status_code directly)."""
        return self.post(
            f"/experiments/{job_id}/metrics",
            {"metric_name": metric_name, "fraction_complete": fraction_complete, "metric_value": metric_value},
        )

    def metric_distinct_count(self, job_id: str, metric_name: str) -> int:
        """How many distinct values of a metric were recorded -- e.g. the number of gang attempts
        that actually ran, each reporting its own index under the same experiment id."""
        return len({v for v in self.metric_values(job_id, metric_name)})

    def experiment_stats(self, pe_id: str, agent_id: str) -> dict:
        return self.get_json(f"/experiments/stats?agent_id={agent_id}&platform_experiment_id={pe_id}")

    def eviction_class_count(self, pe_id: str, agent_id: str, klass: str) -> int | None:
        by_class = self.experiment_stats(pe_id, agent_id).get("evictions_by_class")
        return None if by_class is None else by_class.get(klass, 0)

    def eviction_class_coverage(self, pe_id: str, agent_id: str) -> tuple[int, int, int]:
        stats = self.experiment_stats(pe_id, agent_id)
        by_class = stats.get("evictions_by_class") or {}
        by_reason = stats.get("evictions_by_reason") or {}
        return sum(by_class.values()), sum(by_reason.values()), by_class.get("unclassified", 0)

    # ---- resource catalog ------------------------------------------------------------------------

    def capacity(self, org_id: str = "") -> dict:
        """The clusters one org's jobs can be placed on: the named org, else the one org the token
        is bound to. Staff naming none read every cluster."""
        return self.get_json("/resource-catalog/capacity" + (f"?org_id={org_id}" if org_id else ""))

    def accelerators_free(self, accelerator_type: str) -> int:
        """How many of `accelerator_type` the platform currently reports unused -- the same
        figure admission reads. Ported from tests/lib/api.sh::accelerators_free."""
        want = accelerator_type.lower()
        total = 0
        for cluster in self.capacity().get("clusters") or []:
            for a in cluster.get("accelerators") or []:
                if a["accelerator_type"].lower() == want:
                    total += a["available"]
        return total

    def accelerators_free_settled(self, accelerator_type: str, *, stable_reads: int = 3, budget: float = 60.0) -> int:
        """A free count read only once it has come back the same value `stable_reads` times in a
        row -- COMPLETED is the control plane's verdict on a job, not proof its accelerator is
        back in the schedulable pool; a job can finish and still be draining. Ported from
        tests/lib/api.sh::accelerators_free_settled."""
        import time as _time

        seen: int | None = None
        streak = 0
        v = self.accelerators_free(accelerator_type)
        deadline_at = _time.monotonic() + budget
        while _time.monotonic() < deadline_at:
            v = self.accelerators_free(accelerator_type)
            if v == seen:
                streak += 1
                if streak >= stable_reads:
                    return v
            else:
                seen = v
                streak = 1
            _time.sleep(2)
        return v

    def experiment_data_keys(self, job_id: str) -> list[str]:
        """The keys a job left behind under its own address, live from the store -- ported from
        tests/lib/api.sh's inline listing used by preemption-requeue.sh's checkpoint check."""
        resp = self.experiment_data(job_id)
        resp.raise_for_status()
        return [o["key"] for o in resp.json()]

    # ---- internal cluster-agent-facing endpoints, read for assertions only ---------------------

    def internal_clusters(self) -> list[dict]:
        """GET /internal/clusters/: every cluster that has ever polled and whether it is
        connected right now. Used by fault tests to observe cluster-agent connectivity/liveness
        from the control plane's own point of view."""
        return self.get_json("/internal/clusters/")["clusters"]

    def cluster_absent_from_live_metrics(self, cluster_name: str) -> bool:
        """Ported from tests/scenarios/connectivity-loss.sh::cluster_absent_from_live_metrics:
        true once `cluster_name` no longer appears in /internal/clusters/ at all -- not merely
        marked disconnected, but aged out of the live-heartbeat window entirely."""
        return all(c["cluster_name"] != cluster_name for c in self.internal_clusters())
