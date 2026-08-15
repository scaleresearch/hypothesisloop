"""Package a run dir into the `.sif` the challenge wants.

    uv run python -m fomo_tune.build output/fomo_tune/task1_dwi

Task-agnostic: which task it is comes from `task` in the run's saved config, and the only thing
that varies with it is the module the generated shim imports.
"""

import argparse
import shutil
import subprocess
from pathlib import Path

import torch
from huggingface_hub import hf_hub_download
from omegaconf import OmegaConf

HERE = Path(__file__).parent

PREDICT_SHIM = """
import sys

from fomo_tune.{module} import main

sys.argv = [
    sys.argv[0],
    "predict",
    *sys.argv[1:],
    "--model-dir",
    "/app/model",
    "--ckpt-path",
    "/app/model/backbone.pth",
]

main()
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("run_dir", type=Path, help="a train run dir, holding model/")
    parser.add_argument("--sif", type=Path, help="defaults to <run_dir>/<task>.sif")
    parser.add_argument("--stage", type=Path, help="defaults to <run_dir>/build")
    args = parser.parse_args()

    model_dir = args.run_dir / "model"
    cfg = OmegaConf.load(model_dir / "config.yaml")
    stage = args.stage or args.run_dir / "build"
    sif = args.sif or args.run_dir / f"{cfg.task}.sif"

    shutil.rmtree(stage, ignore_errors=True)
    ignore = shutil.ignore_patterns("__pycache__", "Apptainer.def", "build.py")
    shutil.copytree(HERE, stage / "fomo_tune", ignore=ignore)
    shutil.copytree(HERE.parent / "smri_mae", stage / "smri_mae", ignore=ignore)
    shutil.copytree(model_dir, stage / "model")
    shutil.copy(HERE / "Apptainer.def", stage / "Apptainer.def")
    (stage / "predict.py").write_text(PREDICT_SHIM.format(module=f"main_{cfg.task}"))

    path = cfg.ckpt_path
    if path.startswith("hf://"):
        org, repo, *rest = path.removeprefix("hf://").split("/")
        path = hf_hub_download(f"{org}/{repo}", "/".join(rest))

    # three quarters of the 3.9G checkpoint is optimizer state that inference never reads
    ckpt = torch.load(path, map_location="cpu", weights_only=True, mmap=True)
    torch.save({"model": ckpt["model"], "args": ckpt["args"]}, stage / "model" / "backbone.pth")

    build = ["apptainer", "build", "--fakeroot", "--force", str(sif.resolve()), "Apptainer.def"]
    subprocess.run(build, cwd=stage, check=True)

    # a gigabyte of it is the trimmed checkpoint, and a failed build leaves it for inspection
    shutil.rmtree(stage)
    print(f"built {sif}")


if __name__ == "__main__":
    main()
