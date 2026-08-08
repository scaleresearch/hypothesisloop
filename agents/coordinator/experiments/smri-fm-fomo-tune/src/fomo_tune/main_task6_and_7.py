"""FOMO tasks 6 and 7: linear probing and bias & fairness, both scored off one embedding.

`Task6And7Method` is the part we tune -- which pooling of the frozen encoder we ship. There is no
protocol section: the challenge withholds the labels and fits its own probes, so there is nothing
to cross-validate here. The embedding is the vector tasks 1, 3 and 5 call `features`, and their
out-of-fold scores are the evidence it carries signal.

`export` writes the run dir `build.py` packages; `predict` is the challenge contract, one nifti of
any modality in and one fixed-length float32 `.npy` out.
"""

import argparse
import logging
from dataclasses import dataclass
from pathlib import Path

import nibabel as nib
import numpy as np
import torch
from omegaconf import OmegaConf

from fomo_tune.backbone import load_backbone
from fomo_tune.utils import git_sha, setup_logging

logger = logging.getLogger("fomo_tune")


@dataclass
class Config:
    task: str = "task6_and_7"
    ckpt_path: str = "hf://medarc/walnut/checkpoints/pretrain_full_90_10_h100/checkpoint-last.pth"
    output_root: str = "output/fomo_tune"
    name: str = "task6_and_7"
    device: str = "cuda"


# ---- method: the part we tune -----------------------------------------------------------


class Task6And7Method:
    """Frozen sMRI MAE, mean-pooled tokens over whichever modality arrives."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.backbone, self.transform = load_backbone(cfg.ckpt_path)
        self.device = torch.device(cfg.device)
        self.backbone.to(self.device).eval().requires_grad_(False)

    @torch.inference_mode()
    def predict(self, image: nib.Nifti1Image) -> np.ndarray:
        """(D,) float32. Pooling over the token axis, so D does not depend on the input grid."""
        sample = self.transform(image)
        batch = {key: value[None].to(self.device) for key, value in sample.items()}

        with torch.autocast("cuda", torch.bfloat16, enabled=self.device.type == "cuda"):
            out = self.backbone(batch)

        patch_embeds = out["patch_embeds"]
        token_mask = out["token_mask"].bool().unsqueeze(-1)
        embed = (patch_embeds * token_mask).sum(dim=1) / token_mask.sum(dim=1)
        return embed[0].float().cpu().numpy()

    def save(self, model_dir: Path) -> None:
        """Nothing is fitted, so this is the config alone -- the backbone weights stay wherever
        `ckpt_path` points, and `build.py` is what copies them into a container."""
        model_dir.mkdir(parents=True, exist_ok=True)
        OmegaConf.save(self.cfg, model_dir / "config.yaml")

    @classmethod
    def load(cls, model_dir: Path, **overrides) -> "Task6And7Method":
        """Rebuild the method from `save`. Overrides are Config fields, for what differs between
        here and the container -- the backbone path, the device."""
        cfg = OmegaConf.merge(
            OmegaConf.structured(Config), OmegaConf.load(model_dir / "config.yaml"), overrides
        )
        return cls(cfg)


# ---- entrypoints ------------------------------------------------------------------------


def export(args: argparse.Namespace) -> None:
    """The run dir the other tasks get from `train`, without the fitting there is nothing to do."""
    cfg = OmegaConf.merge(OmegaConf.structured(Config), OmegaConf.from_dotlist(args.overrides))
    run_dir = Path(cfg.output_root) / cfg.name
    run_dir.mkdir(parents=True, exist_ok=True)

    setup_logging(run_dir)
    logger.info(f"run {cfg.name} (git {git_sha()})")
    logger.info(f"config:\n{OmegaConf.to_yaml(cfg).rstrip()}")
    OmegaConf.save(cfg, run_dir / "config.yaml")

    method = Task6And7Method(cfg)
    method.save(run_dir / "model")
    logger.info(f"embedding dim {method.backbone.encoder.patch_embed.out_features}")


def predict(args: argparse.Namespace) -> None:
    """The challenge contract: one nifti path in, one embedding written to `--output`.

    `/app/predict.py` in the container is a shim over this, so what the challenge probes is the
    same vector tasks 1, 3 and 5 score out of fold.
    """
    overrides = {"device": args.device}
    if args.ckpt_path:
        overrides["ckpt_path"] = args.ckpt_path
    method = Task6And7Method.load(args.model_dir, **overrides)

    embedding = method.predict(nib.load(args.input))

    np.save(args.output, embedding)


def main() -> None:
    parser = argparse.ArgumentParser()
    modes = parser.add_subparsers(required=True)

    export_parser = modes.add_parser("export", help="write the run dir a container is built from")
    export_parser.add_argument("overrides", nargs="*", help="config overrides, e.g. device=cpu")
    export_parser.set_defaults(run=export)

    predict_parser = modes.add_parser("predict", help="one image, one embedding")
    predict_parser.add_argument("--input", type=Path, required=True)
    predict_parser.add_argument("--output", type=Path, required=True)
    predict_parser.add_argument("--model-dir", type=Path, default=Path("/app/model"))
    predict_parser.add_argument("--ckpt-path", help="overrides the trained config's backbone path")
    predict_parser.add_argument("--device", default="cuda" if torch.cuda.is_available() else "cpu")
    predict_parser.set_defaults(run=predict)

    args = parser.parse_args()
    args.run(args)


if __name__ == "__main__":
    main()
