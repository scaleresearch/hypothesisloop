"""One typed-enough API client (tests/improve.md #2.4): requests for HTTP, subprocess for `hl` —
the CLI path a real agent uses for registration/signup/submission. Every call returns a dict (or a
small dataclass); assertions live in the test, not buried in a shell string comparison.

Fetch ONE state snapshot per assertion group: `experiment()`/`platform_experiment()` return the
full body so a test can assert on several fields without risking two different moments.
"""
from __future__ import annotations

import json
import subprocess
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import requests
import yaml

REPO_ROOT = Path(__file__).resolve().parents[2]
JOB_SPEC_PATH = REPO_ROOT / "tests" / "workloads" / "generic" / "job.yaml"


def load_yaml(path: Path, overrides: dict[str, Any] | None = None) -> dict[str, Any]:
    """Load a YAML config (mirroring `hl`'s own config shape) and apply a shallow dict of
    overrides on top. `None` in overrides deletes the key, matching mk_body.py's JOB_OVERRIDE_JSON
    convention (the merge/delete escape hatch for fields with no dedicated CLI arg)."""
    doc = yaml.safe_load(path.read_text())
    if overrides:
        for k, v in overrides.items():
            if v is None:
                doc.pop(k, None)
            else:
                doc[k] = v
    return doc


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
    hours: float
    tier: str = "guaranteed"
    job_overrides: dict[str, Any] = field(default_factory=dict)
    metadata_overrides: dict[str, Any] = field(default_factory=dict)

    def to_yaml(self) -> str:
        doc = load_yaml(JOB_SPEC_PATH)
        job = dict(doc.get("job", doc))
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
            "project_id": "",
            "objective": "e2e pytest coverage",
            "theory": "e2e pytest coverage",
            "code_ref": "git://hypothesisloop@" + "a" * 40,
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
    def __init__(self, base_url: str, hl_bin: Path):
        self.base_url = base_url.rstrip("/")
        self.hl_bin = hl_bin
        self.session = requests.Session()

    # ---- hl CLI -----------------------------------------------------------------------------

    def hl(self, *args: str, input: str | None = None) -> str:
        cmd = [str(self.hl_bin), *args]
        proc = subprocess.run(
            cmd,
            input=input,
            capture_output=True,
            text=True,
            env={"API_URL": self.base_url, "PATH": "/usr/bin:/bin:/usr/local/bin"},
            timeout=60,
        )
        if proc.returncode != 0:
            raise HLError(cmd, proc.returncode, (proc.stdout or "") + (proc.stderr or ""))
        return proc.stdout

    def hl_env(self) -> dict[str, str]:
        """The env `hl()`/`hl_expect()` invoke the CLI with -- exposed so a test that needs its own
        subprocess.Popen (e.g. a long-running `hl watch` run concurrently with another action) can
        build an identical child process."""
        return {"API_URL": self.base_url, "PATH": "/usr/bin:/bin:/usr/local/bin"}

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
        return self.session.post(self._url(path), json=json_body, timeout=30, **kw)

    def put(self, path: str, json_body: dict | None = None, **kw) -> requests.Response:
        return self.session.put(self._url(path), json=json_body, timeout=30, **kw)

    def get_json(self, path: str, **kw) -> Any:
        r = self.get(path, **kw)
        r.raise_for_status()
        return r.json()

    def post_json(self, path: str, json_body: dict | None = None, **kw) -> Any:
        r = self.post(path, json_body, **kw)
        r.raise_for_status()
        return r.json()

    # ---- agents -------------------------------------------------------------------------------

    def register_agent(self, agent_id: str, kind: str = "agent") -> None:
        self.hl("register", "--id", agent_id, "--name", agent_id, "--kind", kind)

    # ---- platform experiments -------------------------------------------------------------------

    def create_platform_experiment(
        self,
        name: str,
        budget_accelerator_hours: float,
        max_agents: int,
        report_interval_seconds: int = 10,
        metrics: list[dict] | None = None,
        stages: list[dict] | None = None,
        max_concurrent_accelerators: int | None = None,
    ) -> str:
        body: dict[str, Any] = {
            "name": name,
            "description": name,
            "budget_accelerator_hours": budget_accelerator_hours,
            "max_agents": max_agents,
            "report_interval_seconds": report_interval_seconds,
            "metrics": metrics or [{"key": "val_accuracy", "direction": "maximize"}],
            "starts_at": "0001-01-01T00:00:00Z",
            "ends_at": "0001-01-01T00:00:00Z",
        }
        if stages is not None:
            body["stages"] = stages
        if max_concurrent_accelerators is not None:
            body["max_concurrent_accelerators"] = max_concurrent_accelerators
        out = self.hl("platform-experiments", "create", "-", input=yaml.safe_dump(body, sort_keys=False))
        return json.loads(out)["id"]

    # ---- clusters (autoscaler.md) -----------------------------------------------------------

    def list_internal_clusters(self) -> list[dict]:
        """`GET /internal/clusters` -- id/name/autoscaler_enabled for every cluster the control
        plane has heard from, per autoscaler.md's cluster-identity design."""
        return self.get_json("/internal/clusters")

    def cluster_id_for_name(self, cluster_name: str) -> str | None:
        for c in self.list_internal_clusters():
            if c.get("cluster_name") == cluster_name:
                return c.get("cluster_id") or None
        return None

    def put_cluster_settings(
        self, cluster_id: str, *, scale_up_timeout_seconds: int | None = None,
        max_speculative_accelerators: int | None = None,
    ) -> requests.Response:
        body: dict[str, Any] = {}
        if scale_up_timeout_seconds is not None:
            body["scale_up_timeout_seconds"] = scale_up_timeout_seconds
        if max_speculative_accelerators is not None:
            body["max_speculative_accelerators"] = max_speculative_accelerators
        return self.put(f"/clusters/{cluster_id}/settings", body)

    def put_cluster_settings_ok(
        self, cluster_id: str, *, scale_up_timeout_seconds: int | None = None,
        max_speculative_accelerators: int | None = None,
    ) -> None:
        r = self.put_cluster_settings(
            cluster_id,
            scale_up_timeout_seconds=scale_up_timeout_seconds,
            max_speculative_accelerators=max_speculative_accelerators,
        )
        r.raise_for_status()

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
        return self.post(f"/platform-experiments/{pe_id}/signup", payload)

    def signup_ok(self, pe_id: str, agent_id: str, role: str | None = None, quota_tier: str | None = "guaranteed") -> None:
        r = self.signup(pe_id, agent_id, role, quota_tier)
        r.raise_for_status()

    def signup_role(self, pe_id: str, agent_id: str) -> tuple[int, str | None]:
        r = self.get(f"/platform-experiments/{pe_id}/signups/{agent_id}")
        if r.status_code == 404:
            return 404, None
        r.raise_for_status()
        return r.status_code, r.json().get("role")

    def start_platform_experiment(self, pe_id: str) -> None:
        self.hl("platform-experiments", "start", "--id", pe_id)

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
        # Idempotent, bounded, and best-effort on cleanup: a test's fixture teardown must not raise
        # over a PE that failed to create or was already closed by the test body.
        try:
            self.post(f"/platform-experiments/{pe_id}/close", {"top_results": []})
        except requests.RequestException:
            pass

    def platform_experiment_quotas(self, pe_id: str) -> list[dict]:
        return self.get_json(f"/platform-experiments/{pe_id}/quotas")

    def quota_field(self, pe_id: str, agent_id: str, field_name: str) -> float:
        for q in self.platform_experiment_quotas(pe_id):
            if q.get("agent_id") == agent_id:
                return q.get(field_name, 0)
        return 0

    def stages(self, pe_id: str) -> dict:
        return self.get_json(f"/platform-experiments/{pe_id}/stages")

    def results(self, pe_id: str) -> dict:
        return self.get_json(f"/platform-experiments/{pe_id}/results")

    # ---- hypotheses ----------------------------------------------------------------------------

    def post_hypothesis(
        self, pe_id: str, text: str = "", agent_id: str = "", author: str = ""
    ) -> requests.Response:
        return self.post(
            "/hypotheses",
            {"agent_id": agent_id, "author": author, "platform_experiment_id": pe_id, "text": text or f"e2e {agent_id or author}"},
        )

    def register_hypothesis(self, pe_id: str, agent_id: str, text: str = "") -> dict:
        r = self.post_hypothesis(pe_id, text, agent_id=agent_id)
        r.raise_for_status()
        return r.json()

    def register_human_hypothesis(self, pe_id: str, author: str, text: str) -> dict:
        r = self.post_hypothesis(pe_id, text, author=author)
        r.raise_for_status()
        return r.json()

    def hypothesis(self, hyp_id: str) -> dict:
        return self.get_json(f"/hypotheses/{hyp_id}")

    def hypotheses(self, pe_id: str, limit: int = 200) -> list[dict]:
        return self.get_json(f"/hypotheses?platform_experiment_id={pe_id}&limit={limit}")

    def post_hypothesis_comment(self, hyp_id: str, text: str, agent_id: str = "", author: str = "") -> requests.Response:
        return self.post(f"/hypotheses/{hyp_id}/comments", {"agent_id": agent_id, "author": author, "text": text})

    def set_hypothesis_status(self, hyp_id: str, agent_id: str, status: str) -> requests.Response:
        return self.post(f"/hypotheses/{hyp_id}/status", {"agent_id": agent_id, "status": status})

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
    ) -> str:
        """Registers a hypothesis for the agent (unless hyp_id names an existing row) and submits
        the job through the `hl` CLI -- the real submission path an agent uses."""
        if hyp_id is None:
            hyp_id = self.register_hypothesis(pe_id, agent_id, hyp_text)["id"]
        jid = job_id or self._job_id()
        req = JobRequest(
            id=jid,
            agent_id=agent_id,
            platform_experiment_id=pe_id,
            hypothesis_id=hyp_id,
            hours=hours,
            tier=tier,
            job_overrides=job_overrides or {},
            metadata_overrides=metadata_overrides or {},
        )
        self.hl("job", "submit", "--agent", agent_id, "-", input=req.to_yaml())
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
            hours=hours,
            tier=tier,
            job_overrides=job_overrides or {},
        )
        body = yaml.safe_load(req.to_yaml())
        r = self.post("/experiments", body)
        return r.status_code, jid

    def submission_body_for_id(self, job_id: str, pe_id: str, agent_id: str, tier: str = "guaranteed", hours: float = 0.02) -> dict:
        hyp_id = self.register_hypothesis(pe_id, agent_id, "")["id"]
        req = JobRequest(id=job_id, agent_id=agent_id, platform_experiment_id=pe_id, hypothesis_id=hyp_id, hours=hours, tier=tier)
        return yaml.safe_load(req.to_yaml())

    def post_experiment_body(self, body: dict) -> requests.Response:
        return self.post("/experiments", body)

    def experiment(self, job_id: str) -> dict:
        """One snapshot of an experiment's full state (tests/improve.md #2.4: fetch once per
        assertion group, never one HTTP call per field)."""
        r = self.get(f"/experiments/{job_id}")
        r.raise_for_status()
        return r.json()

    def experiment_status_code(self, job_id: str) -> int:
        return self.get(f"/experiments/{job_id}").status_code

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
            f"/experiments?needs_summary=true&agent={agent_id}&platform_experiment_id={pe_id}&limit={limit}"
        )
        return [e["id"] for e in docs]

    def logs(self, job_id: str, n: int = 200) -> str:
        r = self.get(f"/experiments/{job_id}/logs", params={"n": n})
        return r.text if r.ok else ""

    def metrics(self, job_id: str) -> list[dict]:
        return self.get_json(f"/experiments/{job_id}/metrics")

    def metric_values(self, job_id: str, metric_name: str) -> list[float]:
        return [p["metric_value"] for p in self.metrics(job_id) if p.get("metric_name") == metric_name]

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
        return self.get_json(f"/experiments/stats?agent={agent_id}&platform_experiment_id={pe_id}")

    def eviction_class_count(self, pe_id: str, agent_id: str, klass: str) -> int | None:
        by_class = self.experiment_stats(pe_id, agent_id).get("evictions_by_class")
        return None if by_class is None else by_class.get(klass, 0)

    def eviction_class_coverage(self, pe_id: str, agent_id: str) -> tuple[int, int, int]:
        stats = self.experiment_stats(pe_id, agent_id)
        by_class = stats.get("evictions_by_class") or {}
        by_reason = stats.get("evictions_by_reason") or {}
        return sum(by_class.values()), sum(by_reason.values()), by_class.get("unclassified", 0)

    # ---- donations -----------------------------------------------------------------------------

    def create_donation(self, agent_id: str, pe_id: str, credits_want: float, reason: str) -> str:
        return self.post_json(
            "/donations",
            {"agent_id": agent_id, "platform_experiment_id": pe_id, "credits_want": credits_want, "reason": reason},
        )["id"]

    def fulfill_donation(self, donation_id: str, donor_agent_id: str) -> requests.Response:
        return self.post(f"/donations/{donation_id}/fulfill", {"donor_agent_id": donor_agent_id})

    def cancel_donation(self, donation_id: str) -> requests.Response:
        return self.post(f"/donations/{donation_id}/cancel", {})

    # ---- resource catalog ------------------------------------------------------------------------

    def capacity(self) -> dict:
        return self.get_json("/resource-catalog/capacity")

    def accelerators_free(self, accelerator_type: str) -> int:
        """How many of `accelerator_type` the platform currently reports unused -- the same
        figure admission reads. Ported from tests/lib/api.sh::accelerators_free."""
        want = accelerator_type.lower()
        total = 0
        for cluster in self.capacity().get("clusters") or []:
            for a in cluster.get("accelerators") or []:
                if (a.get("accelerator_type") or "").lower() == want:
                    total += int(a.get("available") or 0)
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
        if resp.status_code != 200:
            return []
        return [o.get("key") for o in resp.json() if o.get("key")]

    # ---- internal cluster-agent-facing endpoints, read for assertions only ---------------------

    def internal_clusters(self) -> list[dict]:
        """GET /internal/clusters/: every cluster that has ever polled and whether it is
        connected right now. Used by fault tests to observe cluster-agent connectivity/liveness
        from the control plane's own point of view."""
        return self.get_json("/internal/clusters/").get("clusters", [])

    def cluster_absent_from_live_metrics(self, cluster_name: str) -> bool:
        """Ported from tests/scenarios/connectivity-loss.sh::cluster_absent_from_live_metrics:
        true once `cluster_name` no longer appears in /internal/clusters/ at all -- not merely
        marked disconnected, but aged out of the live-heartbeat window entirely."""
        return all(c.get("cluster_name") != cluster_name for c in self.internal_clusters())
