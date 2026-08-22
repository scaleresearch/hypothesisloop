#!/usr/bin/env python3
"""
Durable-data e2e workload: one stage of a chain, either the one that writes a checkpoint or the
one that reads it back.

Two modes, one file, because the pair only means anything together:

  STAGE_MODE=write  every node writes <own prefix>/checkpoint-rank<global rank>.txt, whose body
                    is CHECKPOINT_VALUE. Reports its own global rank as a metric.
  STAGE_MODE=read   finds the parent stage's rank-0 checkpoint under the platform experiment's
                    SHARED prefix, reports its contents as a metric, and then probes what the
                    job's credentials may and may not do — see the four status metrics below.

S3 is spoken directly over stdlib HTTP with a hand-rolled SigV4 signer, for the same reason
controlplane/shared/objectstore does it: the workload image is python:3.13-slim with no SDK in it
(and no `aws`/`mc` binary either), and two request shapes do not justify one. Signing here is not
incidental to what is under test — the session token is part of the signature, so a request this
file sends is a request made with the scoped session the platform handed the job, not with some
ambient credential.

Every S3 call is signed with AWS_SESSION_TOKEN and the read mode REFUSES TO RUN without one. That
is what makes "a write into the parent's prefix is refused" mean the store enforced the session's
policy: a client holding only key/secret would be refused for a different reason entirely, and
would make the same assertion pass while proving nothing. Read mode therefore reports, from the
one client with the one set of credentials:

  session_token_present   1, or the job exits non-zero — never a silent fallback
  shared_read_status      200: this client CAN read the parent's object across prefixes
  own_prefix_write_status 200: this client CAN write, in its own prefix
  foreign_write_status    403 expected: the same client, same credentials, same endpoint, one
                          key away from the object it just read successfully, denied

Environment (injected by the runtime, see runtime/k8s/internal/k8sexec/job_build.go):
  HYPOTHESISLOOP_DATA_URI     s3://bucket/<pe>/<agent>/<experiment>/ — the only writable prefix
  HYPOTHESISLOOP_DATA_SHARED  s3://bucket/<pe>/ — readable across the platform experiment
  AWS_ENDPOINT_URL, AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN
  HYPOTHESISLOOP_RANK_OFFSET / _GROUP_RANK   grouped job: this pod's two rank terms
  RANK / HYPOTHESISLOOP_RANK                 ungrouped job: this pod's rank outright
"""

import hashlib
import hmac
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from datetime import datetime, timezone

EXP_ID = os.environ.get("HYPOTHESISLOOP_EXPERIMENT_ID", "local-test")
API_URL = os.environ.get("HYPOTHESISLOOP_API_URL", "http://localhost:8081")
MODE = os.environ.get("STAGE_MODE", "write")
CHECKPOINT_VALUE = os.environ.get("CHECKPOINT_VALUE", "0")
# The experiment id of the stage whose checkpoint this one reads. Read mode only.
PARENT_ID = os.environ.get("STAGE_PARENT_ID", "")
DURATION = int(os.environ.get("HYPOTHESISLOOP_DURATION_SECONDS", "10"))

DATA_URI = os.environ.get("HYPOTHESISLOOP_DATA_URI", "")
DATA_SHARED = os.environ.get("HYPOTHESISLOOP_DATA_SHARED", "")
ENDPOINT = (os.environ.get("AWS_ENDPOINT_URL") or "").rstrip("/")
REGION = os.environ.get("AWS_REGION", "us-east-1")
ACCESS_KEY = os.environ.get("AWS_ACCESS_KEY_ID", "")
SECRET_KEY = os.environ.get("AWS_SECRET_ACCESS_KEY", "")
SESSION_TOKEN = os.environ.get("AWS_SESSION_TOKEN", "")

EMPTY_SHA256 = hashlib.sha256(b"").hexdigest()


def resolve_rank() -> int:
    """This node's JOB-GLOBAL rank — see train_distributed.py's resolve_rank for why a grouped
    pod is given the two terms rather than the sum."""
    offset = os.environ.get("HYPOTHESISLOOP_RANK_OFFSET")
    group_rank = os.environ.get("HYPOTHESISLOOP_GROUP_RANK")
    if offset is not None and group_rank is not None:
        return int(offset) + int(group_rank)
    return int(os.environ.get("RANK", os.environ.get("HYPOTHESISLOOP_RANK", "0")))


RANK = resolve_rank()


def log(message: str) -> None:
    print(f"[{MODE} rank {RANK}] {message}", flush=True)


def post_metric(name: str, value: float, fraction: float = 1.0) -> None:
    url = f"{API_URL}/experiments/{EXP_ID}/metrics"
    payload = json.dumps(
        {"metric_name": name, "fraction_complete": fraction, "metric_value": value}
    ).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception as e:  # reporting must never take down the work it reports on
        print(f"  [warn] metric POST failed: {e}", file=sys.stderr, flush=True)


def split_s3_uri(uri: str) -> tuple[str, str]:
    """s3://bucket/prefix/ -> (bucket, prefix/)."""
    if not uri.startswith("s3://"):
        raise SystemExit(f"expected an s3:// address, got {uri!r}")
    rest = uri[len("s3://") :]
    bucket, _, key = rest.partition("/")
    return bucket, key


def sign(method: str, path: str, query: dict[str, str], body: bytes) -> dict[str, str]:
    """SigV4 headers for one request. The session token is signed, not merely attached: the
    store rejects a signature that does not cover it, so a request that reaches the store at all
    is one made under the job's scoped session."""
    payload_hash = hashlib.sha256(body).hexdigest() if body else EMPTY_SHA256
    now = datetime.now(timezone.utc)
    amz_date = now.strftime("%Y%m%dT%H%M%SZ")
    date_stamp = now.strftime("%Y%m%d")
    host = urllib.parse.urlparse(ENDPOINT).netloc

    canonical_query = "&".join(
        f"{urllib.parse.quote(k, safe='')}={urllib.parse.quote(query[k], safe='')}"
        for k in sorted(query)
    )
    to_sign = {
        "host": host,
        "x-amz-content-sha256": payload_hash,
        "x-amz-date": amz_date,
    }
    # The session token is signed rather than merely attached, which is what makes a refusal
    # meaningful: the store validates the signature over it, so this request is unambiguously
    # made under the session the platform scoped to this job.
    if SESSION_TOKEN:
        to_sign["x-amz-security-token"] = SESSION_TOKEN
    signed_headers = ";".join(sorted(to_sign))
    canonical_headers = "".join(f"{k}:{to_sign[k]}\n" for k in sorted(to_sign))
    canonical_request = "\n".join(
        [method, path, canonical_query, canonical_headers, signed_headers, payload_hash]
    )
    scope = f"{date_stamp}/{REGION}/s3/aws4_request"
    string_to_sign = "\n".join(
        [
            "AWS4-HMAC-SHA256",
            amz_date,
            scope,
            hashlib.sha256(canonical_request.encode()).hexdigest(),
        ]
    )

    def mac(key: bytes, data: str) -> bytes:
        return hmac.new(key, data.encode(), hashlib.sha256).digest()

    key = mac(("AWS4" + SECRET_KEY).encode(), date_stamp)
    for part in (REGION, "s3", "aws4_request"):
        key = mac(key, part)
    signature = hmac.new(key, string_to_sign.encode(), hashlib.sha256).hexdigest()
    return {
        **{k: v for k, v in to_sign.items() if k != "host"},
        "Authorization": (
            f"AWS4-HMAC-SHA256 Credential={ACCESS_KEY}/{scope}, "
            f"SignedHeaders={signed_headers}, Signature={signature}"
        ),
    }


def s3(method: str, path: str, query: dict[str, str] | None = None, body: bytes = b"") -> tuple[int, bytes]:
    """One S3 request. Returns (status, body) for ANY HTTP status, including 403 — a refusal is
    an answer this workload has to report, never an exception that reads like a broken client.
    Anything below HTTP (DNS, connect, TLS) still raises, because that is not a refusal and must
    not be reported as one."""
    query = query or {}
    headers = sign(method, path, query, body)
    url = ENDPOINT + path
    if query:
        url += "?" + urllib.parse.urlencode(query)
    req = urllib.request.Request(url, data=body or None, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def object_path(bucket: str, key: str) -> str:
    """Path-style addressing, matching what the control plane's own client uses. Keys here are
    ids and fixed filenames — [a-z0-9-_./] only — so no segment needs escaping; anything that
    did would have to be escaped identically in the signature and in the URL."""
    return f"/{bucket}/{key}"


def list_keys(bucket: str, prefix: str) -> list[str]:
    status, body = s3("GET", f"/{bucket}", {"list-type": "2", "prefix": prefix, "max-keys": "1000"})
    if status != 200:
        raise SystemExit(f"listing {prefix!r} failed with status {status}: {body[:400]!r}")
    # MinIO namespaces its response; matching on the local tag name keeps this independent of
    # which S3 implementation is behind the endpoint.
    return [
        el.text
        for el in ET.fromstring(body).iter()
        if el.tag.split("}")[-1] == "Key" and el.text
    ]


def hold_open() -> None:
    """Report progress for the declared duration so the job looks like a real run to the
    platform's silence checks rather than exiting the instant it starts."""
    steps = max(1, DURATION // 5)
    for step in range(steps):
        time.sleep(5)
        post_metric("val_accuracy", 0.5 + 0.4 * (step + 1) / steps, (step + 1) / steps)


def do_write() -> None:
    bucket, prefix = split_s3_uri(DATA_URI)
    # One key per global rank. Two ranks colliding on one key is exactly what a group-local rank
    # would produce, and the scenario counts the objects this leaves behind.
    key = f"{prefix}checkpoint-rank{RANK}.txt"
    status, body = s3("PUT", object_path(bucket, key), body=CHECKPOINT_VALUE.encode())
    log(f"wrote s3://{bucket}/{key} -> {status}")
    if status != 200:
        raise SystemExit(f"writing own checkpoint failed with status {status}: {body[:400]!r}")
    post_metric("stage_rank", float(RANK))
    post_metric("checkpoint_write_status", float(status))
    hold_open()


def do_read() -> None:
    # No token, no run. A client with only key/secret would be refused a cross-prefix write by
    # the store as well — for a completely different reason — so a read stage that quietly ran
    # without a session would report a passing foreign_write_status that means nothing.
    if not SESSION_TOKEN:
        raise SystemExit(
            "AWS_SESSION_TOKEN is empty: this job did not receive a scoped session, so nothing "
            "it observes about what it may write says anything about the scoping"
        )
    post_metric("session_token_present", 1.0)

    shared_bucket, shared_prefix = split_s3_uri(DATA_SHARED)
    if not PARENT_ID:
        raise SystemExit("STAGE_PARENT_ID is unset: there is no parent stage to read from")
    keys = list_keys(shared_bucket, shared_prefix)
    # The parent's prefix is <pe>/<its agent>/<its experiment>/, and this stage is told only the
    # experiment id — the agent segment is the parent's, not this job's, so the key is found by
    # listing the shared prefix rather than assembled from what this job happens to know.
    parent_keys = [k for k in keys if f"/{PARENT_ID}/" in k]
    if not parent_keys:
        raise SystemExit(
            f"no object under {shared_prefix!r} belongs to parent {PARENT_ID}; "
            f"the shared prefix holds {keys[:20]}"
        )
    rank0 = [k for k in parent_keys if k.endswith("checkpoint-rank0.txt")]
    if not rank0:
        raise SystemExit(f"parent {PARENT_ID} left no rank-0 checkpoint; it left {parent_keys}")
    parent_key = rank0[0]

    status, body = s3("GET", object_path(shared_bucket, parent_key))
    log(f"read s3://{shared_bucket}/{parent_key} -> {status} {body[:64]!r}")
    post_metric("shared_read_status", float(status))
    if status != 200:
        raise SystemExit(f"reading the parent's checkpoint failed with status {status}")
    post_metric("checkpoint_value", float(body.decode().strip()))

    # The same client, the same credentials, one key away: this one is inside the prefix this
    # job's session was scoped to, so it must succeed. If it did not, the refusal below would be
    # about a broken client rather than about the store's policy.
    own_bucket, own_prefix = split_s3_uri(DATA_URI)
    own_status, own_body = s3(
        "PUT", object_path(own_bucket, f"{own_prefix}read-stage-marker.txt"), body=b"read stage was here"
    )
    log(f"wrote own prefix marker -> {own_status}")
    post_metric("own_prefix_write_status", float(own_status))
    if own_status != 200:
        raise SystemExit(f"writing this job's OWN prefix failed with {own_status}: {own_body[:400]!r}")

    # And the refusal itself: the parent's prefix, reached by replacing only the filename of the
    # key this job just read successfully. Nothing differs but the prefix.
    intruder_key = parent_key.rsplit("/", 1)[0] + "/overwritten-by-a-rival.txt"
    foreign_status, foreign_body = s3(
        "PUT", object_path(shared_bucket, intruder_key), body=b"this must not land"
    )
    log(f"attempted write into the parent's prefix -> {foreign_status} {foreign_body[:200]!r}")
    post_metric("foreign_write_status", float(foreign_status))
    hold_open()


def main() -> None:
    for name, value in (("AWS_ENDPOINT_URL", ENDPOINT), ("HYPOTHESISLOOP_DATA_URI", DATA_URI)):
        if not value:
            raise SystemExit(f"{name} is empty: the platform did not hand this job an address")
    if MODE == "write":
        do_write()
    elif MODE == "read":
        do_read()
    else:
        raise SystemExit(f"STAGE_MODE={MODE!r}, want write or read")
    log("done")


if __name__ == "__main__":
    main()
