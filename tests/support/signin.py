"""A person's sign-in on the local stack: an ES256 JWT of the local identity provider, signed with
the dev key the make e2e targets export (HL_DEV_JWT_KEY, with its issuer, audience and key id).
"""
from __future__ import annotations

import base64
import json
import os
import subprocess
import time


def _b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def _raw_signature(der: bytes) -> bytes:
    """An ECDSA signature as JWS wants it: r and s, 32 bytes each, from openssl's DER pair."""
    assert der[0] == 0x30 and der[2] == 0x02, "not a DER ECDSA signature"
    r_len = der[3]
    r = der[4:4 + r_len]
    assert der[4 + r_len] == 0x02
    s_len = der[5 + r_len]
    s = der[6 + r_len:6 + r_len + s_len]
    return b"".join(int.from_bytes(part, "big").to_bytes(32, "big") for part in (r, s))


def jwt(subject: str, name: str, lifetime_seconds: int = 600) -> str:
    """The token of a person signed in as `subject`, good for `lifetime_seconds`."""
    body = {"iss": os.environ["HL_DEV_JWT_ISSUER"], "aud": os.environ["HL_DEV_JWT_AUDIENCE"],
            "exp": int(time.time()) + lifetime_seconds, "sub": subject, "name": name}
    header = {"alg": "ES256", "kid": os.environ["HL_DEV_JWT_KID"], "typ": "JWT"}
    signing_input = f"{_b64(json.dumps(header).encode())}.{_b64(json.dumps(body).encode())}"
    der = subprocess.run(["openssl", "dgst", "-sha256", "-sign", os.environ["HL_DEV_JWT_KEY"]],
                         input=signing_input.encode(), capture_output=True, check=True).stdout
    return f"{signing_input}.{_b64(_raw_signature(der))}"
