import logging
import random
import subprocess
import sys
from pathlib import Path

import numpy as np
import torch

logger = logging.getLogger("fomo_tune")


def set_seed(seed: int) -> None:
    random.seed(seed)
    np.random.seed(seed)
    torch.manual_seed(seed)


def git_sha() -> str:
    kwargs = dict(cwd=Path(__file__).parent, capture_output=True, text=True, check=True)
    sha = subprocess.run(["git", "rev-parse", "--short", "HEAD"], **kwargs).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain", "-uno"], **kwargs).stdout.strip()
    return f"{sha}-dirty" if dirty else sha


def setup_logging(run_dir: Path) -> None:
    handlers = [logging.StreamHandler(sys.stdout), logging.FileHandler(run_dir / "log.txt")]
    logger.setLevel(logging.INFO)
    logger.handlers.clear()
    for handler in handlers:
        handler.setFormatter(logging.Formatter("%(asctime)s %(message)s", datefmt="%H:%M:%S"))
        logger.addHandler(handler)
    logger.propagate = False
