"""S3 from the test host with a given key, through the signer the workload itself uses
(tests/workloads/generic/data_stage.py), so the tests carry one signer."""
from __future__ import annotations

import urllib.parse
from pathlib import Path
from types import ModuleType

_DATA_STAGE = Path(__file__).resolve().parents[1] / "workloads" / "generic" / "data_stage.py"


def client(env: dict[str, str]) -> ModuleType:
    """A data_stage module signing with the key in env (the variables `hl data-key` prints). The
    endpoint's host is the address containers reach the store at; the test host reaches the same
    published port on localhost."""
    module = ModuleType("data_stage_client")
    exec(compile(_DATA_STAGE.read_text(), str(_DATA_STAGE), "exec"), module.__dict__)
    port = urllib.parse.urlparse(env["AWS_ENDPOINT_URL"]).port
    module.ENDPOINT = f"http://localhost:{port}"
    module.REGION = env["AWS_REGION"]
    module.ACCESS_KEY = env["AWS_ACCESS_KEY_ID"]
    module.SECRET_KEY = env["AWS_SECRET_ACCESS_KEY"]
    return module
