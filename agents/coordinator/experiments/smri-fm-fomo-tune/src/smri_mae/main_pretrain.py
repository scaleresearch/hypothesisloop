# Copyright (c) Sophont, Inc
# This source code is licensed under the Apache License, Version 2.0
#
# References:
# deit: https://github.com/facebookresearch/deit/blob/main/main.py
# capi: https://github.com/facebookresearch/capi/blob/main/train_capi.py

import argparse
import datetime
import json
import math
import os
import queue
import random
import shutil
import subprocess
import threading
import time
from contextlib import nullcontext
from functools import partial
from itertools import islice
from pathlib import Path
from typing import Iterable, Sequence

import torch
import torch.nn as nn
import wandb
import webdataset as wds
from omegaconf import DictConfig, OmegaConf
from PIL import Image

from matplotlib import pyplot as plt
from torch import Tensor

import data.mri_data as mri_data
import smri_mae.model_mae as models_mae
import smri_mae.utils as ut
import smri_mae.visualization as vis

DEFAULT_CONFIG = Path(__file__).parent / "config/default_pretrain.yaml"

MODELS_DICT = models_mae.__dict__


def main(args: DictConfig):
    # setup
    ut.init_distributed_mode(args)
    global_rank = ut.get_rank()
    is_master = global_rank == 0
    world_size = ut.get_world_size()
    device = torch.device(args.device)
    ut.configure_flash_sdpa()
    ut.random_seed(args.seed, rank=global_rank)

    if args.name and not args.output_dir.endswith(args.name):
        args.output_dir = f"{args.output_dir}/{args.name}"
    output_dir = Path(args.output_dir)

    if is_master:
        output_dir.mkdir(parents=True, exist_ok=True)
        out_cfg_path = output_dir / "config.yaml"
        if out_cfg_path.exists():
            prev_cfg = OmegaConf.load(out_cfg_path)
            assert args == prev_cfg, "current config doesn't match previous config"
        else:
            OmegaConf.save(args, out_cfg_path)

        if args.wandb:
            wandb.init(
                entity=args.wandb_entity,
                project=args.wandb_project,
                name=args.name,
                notes=args.notes,
                config=OmegaConf.to_container(args),
            )

    ut.setup_for_distributed(log_path=output_dir / "log.txt")

    print("pretraining 3D ViTMAE")
    print(f"start: {datetime.datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    print(f"cwd: {Path.cwd()}")
    print(ut.get_sha())
    print("config:", OmegaConf.to_yaml(args), sep="\n")

    # data loaders
    train_loader, eval_loaders = create_data_loaders(args)

    # model
    model = MODELS_DICT[args.model](
        img_size=args.img_size,
        in_chans=args.get("in_chans", 1),
        patch_size=args.patch_size,
        **(args.get("model_kwargs") or {}),
    )
    model.to(device)
    print("model:", model, sep="\n")
    num_params = sum(p.numel() for p in model.parameters() if p.requires_grad)
    print(f"num params: {num_params / 1e6:.1f}M")

    model_without_ddp = model
    if args.distributed:
        model = torch.nn.parallel.DistributedDataParallel(
            model,
            device_ids=[args.gpu],
            gradient_as_bucket_view=True,
        )
        model_without_ddp = model.module

    # optimizer
    total_batch_size = args.batch_size * args.accum_iter * world_size
    print(
        f"total batch size: {total_batch_size} = "
        f"{args.batch_size} bs per gpu x {args.accum_iter} accum x {world_size} gpus"
    )

    if not args.get("lr"):
        args.lr = args.base_lr * total_batch_size / 256
        print(f"lr: {args.lr:.2e} = {args.base_lr:.2e} x {total_batch_size} / 256")
    else:
        print(f"lr: {args.lr:.2e}")

    param_groups = ut.get_param_groups(model)
    ut.update_lr(param_groups, args.lr)
    ut.update_wd(param_groups, args.weight_decay)
    # cast or else it corrupts the checkpoint
    betas = tuple(args.betas) if args.betas is not None else None
    optimizer = torch.optim.AdamW(param_groups, betas=betas, fused=True)

    epoch_num_batches = len(train_loader)
    steps_per_epoch = math.ceil(epoch_num_batches / args.accum_iter)
    total_steps = args.epochs * steps_per_epoch
    warmup_steps = args.warmup_epochs * steps_per_epoch
    lr_schedule = ut.WarmupThenCosine(
        base_value=args.lr,
        final_value=args.min_lr,
        total_iters=total_steps,
        warmup_iters=warmup_steps,
    )
    print(f"full schedule: epochs = {args.epochs} (steps = {total_steps})")
    print(f"warmup: epochs = {args.warmup_epochs} (steps = {warmup_steps})")

    # loss scaling not needed for bfloat16 (according to timm)
    if args.amp and args.amp_dtype != "bfloat16":
        loss_scaler = torch.GradScaler(device.type)
    else:
        loss_scaler = None

    # load checkpoint/resume training
    ut.load_model(args, model_without_ddp, optimizer, loss_scaler)

    print(f"start training for {args.epochs} epochs")
    start_time = time.monotonic()
    for epoch in range(args.start_epoch, args.epochs):
        train_stats = train_one_epoch(
            args,
            model,
            train_loader,
            optimizer,
            loss_scaler,
            lr_schedule,
            epoch,
            device,
        )
        eval_stats = {}
        eval_plots = {}
        eval_period = args.get("eval_period", 1)
        if eval_period and (epoch % eval_period == 0 or epoch == args.epochs - 1):
            for name, loader in eval_loaders.items():
                stats, plots = evaluate(
                    args,
                    model,
                    loader,
                    epoch,
                    device,
                    eval_name=name,
                )
                eval_stats.update(stats)
                eval_plots.update(plots)

        merged_stats = {"epoch": epoch, **train_stats, **eval_stats}
        if is_master:
            with (output_dir / "log.json").open("a") as f:
                print(json.dumps(merged_stats), file=f)

            for plot_name, img in eval_plots.items():
                plot_name = plot_name.replace("/", "__")
                img.save(output_dir / f"{plot_name}__{epoch:05d}.png")

        ut.save_model(args, epoch, model_without_ddp, optimizer, loss_scaler)
        sync_checkpoints_to_r2(args, output_dir)
        sync_checkpoints_to_hf(args, output_dir, epoch, wait=(epoch == args.epochs - 1))

    if args.distributed:
        torch.distributed.destroy_process_group()

    total_time = time.monotonic() - start_time
    print(f"done! training time: {datetime.timedelta(seconds=int(total_time))}")


def create_data_loaders(args: DictConfig):
    data_loaders = {}
    dataset_names = [args.train_dataset] + args.eval_datasets

    for dataset_name in dataset_names:
        dataset_config = args.datasets[dataset_name].copy()
        drop_last = dataset_config.pop("drop_last")
        is_train = dataset_name == args.train_dataset

        print(f"loading dataset: {dataset_name}\n\n{OmegaConf.to_yaml(dataset_config)}")
        shuffle = dataset_config["shuffle"]
        samples_per_epoch = dataset_config.pop("samples_per_epoch")
        dataset = mri_data.make_sparse_wds_dataset(
            dataset_config["url"],
            shuffle=shuffle,
            buffer_size=dataset_config["buffer_size"],
        )
        num_workers = int(args.num_workers)
        loader_kwargs = {
            "batch_size": args.batch_size,
            "collate_fn": partial(mri_data.collate, include_meta=not is_train),
            "shuffle": False,
            "num_workers": num_workers,
            "persistent_workers": num_workers > 0,
            "pin_memory": True,
            "drop_last": drop_last,
            "prefetch_factor": args.prefetch_factor,
        }
        loader = wds.WebLoader(dataset, **loader_kwargs)
        num_batches = samples_per_epoch // (ut.get_world_size() * args.batch_size)
        loader = loader.with_epoch(num_batches)
        loader = loader.with_length(num_batches, silent=True)

        data_loaders[dataset_name] = loader

    train_loader = data_loaders.pop(args.train_dataset)
    return train_loader, data_loaders


def sync_checkpoints_to_r2(args: DictConfig, output_dir: Path) -> None:
    r2_sync_url = args.get("r2_sync")
    if not r2_sync_url or not ut.is_main_process():
        return

    cmd = ["aws", "s3", "sync", str(output_dir), str(r2_sync_url), "--profile", "r2"]
    print(f"syncing checkpoints to R2: {output_dir} -> {r2_sync_url}")
    subprocess.run(cmd, check=True)


_hf_upload_queue: "queue.Queue | None" = None
_hf_upload_thread: threading.Thread | None = None
_hf_staging_dir: Path | None = None

_HF_METADATA_PATTERNS = ("*.yaml", "*.json", "*.txt", "*.png")


def _hf_upload_worker(hf_repo_id: str, subfolder: str, q: "queue.Queue") -> None:
    from huggingface_hub import HfApi

    api = HfApi()
    while True:
        item = q.get()
        if item is None:
            q.task_done()
            return
        staging_dir, checkpoint_name, epoch = item
        try:
            print(f"syncing checkpoint (epoch {epoch}) to HF Hub: {hf_repo_id}/{subfolder}")
            api.upload_folder(
                folder_path=str(staging_dir),
                path_in_repo=subfolder,
                repo_id=hf_repo_id,
                repo_type="model",
            )
            # keep only the checkpoint we just uploaded on the Hub -- at most
            # one ever lives there, regardless of local max_checkpoints.
            prefix = f"{subfolder}/"
            for remote_path in api.list_repo_files(hf_repo_id, repo_type="model"):
                if not remote_path.startswith(prefix):
                    continue
                name = remote_path[len(prefix) :]
                if (
                    name.startswith("checkpoint-")
                    and name.endswith(".pth")
                    and name != checkpoint_name
                ):
                    print(f"removing superseded checkpoint from HF Hub: {remote_path}")
                    api.delete_file(path_in_repo=remote_path, repo_id=hf_repo_id, repo_type="model")
        except Exception as e:
            print(f"HF Hub sync failed for epoch {epoch}: {e}")
        finally:
            shutil.rmtree(staging_dir, ignore_errors=True)
            q.task_done()


def sync_checkpoints_to_hf(
    args: DictConfig, output_dir: Path, epoch: int, wait: bool = False
) -> None:
    """Queue this epoch's checkpoint (plus config/log/eval-plot files) for
    upload to the HF Hub repo/subfolder named by hf_repo_id/hf_subfolder,
    uploaded by a single persistent background worker thread, so training is
    never blocked on the network.

    No-op if hf_repo_id isn't set, or if save_model didn't checkpoint this
    epoch (not on checkpoint_period).

    Staging: everything about to be uploaded is snapshotted into a private
    per-epoch folder under output_dir/.hf_staging/ the moment it's ready --
    the checkpoint via hardlink (same filesystem, instant, no extra disk
    space even for a multi-GB file), the small metadata files (log.json,
    log.txt, config.yaml, eval plot pngs) via a real copy, since log.json/
    log.txt keep growing every epoch and a hardlink would just point at
    that same still-changing file. Without this, the background worker
    could read a metadata file mid-mutation by the next epoch and produce
    an upload whose content hash doesn't match what the Hub expects,
    which HF rejects outright rather than silently corrupting anything.
    Freezing everything into its own snapshot removes that race, and lets
    save_model rotate/delete the original checkpoint immediately -- no
    blocking wait is needed for that anymore either.

    Hub-side retention: after each upload finishes, the worker deletes every
    other checkpoint file on the Hub, so at most one checkpoint -- whichever
    upload most recently finished -- ever lives there, independent of local
    max_checkpoints. Pass wait=True (done automatically on the final epoch,
    see main()) to block until this specific upload finishes and the worker
    thread exits, so the run's last checkpoint is guaranteed to be the one
    left on the Hub when the process exits.
    """
    global _hf_upload_queue, _hf_upload_thread, _hf_staging_dir
    hf_repo_id = args.get("hf_repo_id")
    if not hf_repo_id or not ut.is_main_process():
        return

    checkpoint_path = output_dir / f"checkpoint-{epoch:05d}.pth"
    if not checkpoint_path.exists():
        return

    if _hf_upload_thread is None:
        _hf_upload_queue = queue.Queue()
        _hf_staging_dir = output_dir / ".hf_staging"
        _hf_staging_dir.mkdir(exist_ok=True)
        subfolder = args.get("hf_subfolder") or args.name
        _hf_upload_thread = threading.Thread(
            target=_hf_upload_worker,
            args=(hf_repo_id, subfolder, _hf_upload_queue),
            daemon=False,
        )
        _hf_upload_thread.start()

    epoch_staging_dir = _hf_staging_dir / f"epoch_{epoch:05d}"
    shutil.rmtree(epoch_staging_dir, ignore_errors=True)
    epoch_staging_dir.mkdir(exist_ok=True)
    os.link(checkpoint_path, epoch_staging_dir / checkpoint_path.name)
    for pattern in _HF_METADATA_PATTERNS:
        for src in output_dir.glob(pattern):
            shutil.copy2(src, epoch_staging_dir / src.name)

    _hf_upload_queue.put((epoch_staging_dir, checkpoint_path.name, epoch))

    if wait:
        _hf_upload_queue.join()
        _hf_upload_queue.put(None)
        _hf_upload_thread.join()
        _hf_upload_thread = None


def train_one_epoch(
    args: DictConfig,
    model: nn.Module,
    data_loader: Iterable,
    optimizer: torch.optim.Optimizer,
    loss_scaler: torch.GradScaler | None,
    lr_schedule: Sequence[float],
    epoch: int,
    device: torch.device,
):
    model.train()

    metric_logger = ut.MetricLogger(delimiter="  ")
    metric_logger.add_meter("lr", ut.SmoothedValue(window_size=1, fmt="{value:.6f}"))
    metric_logger.add_meter("grad", ut.SmoothedValue())
    header = f"Train: [{epoch}]"
    log_wandb = args.wandb and ut.is_main_process()

    epoch_num_batches = len(data_loader)
    steps_per_epoch = math.ceil(epoch_num_batches / args.accum_iter)

    print_freq = args.get("print_freq", 100) if not args.debug else 1
    num_batches = epoch_num_batches if not args.debug else 10
    amp_dtype = getattr(torch, args.amp_dtype)
    use_cuda = device.type == "cuda"
    if use_cuda and args.presend_cuda:
        data_loader = ut.pre_send_to_cuda_wrapper(
            data_loader, device, dtype_map={torch.float16: amp_dtype}
        )

    optimizer.zero_grad()

    for batch_idx, batch in enumerate(
        metric_logger.log_every(data_loader, print_freq, header, total_steps=num_batches)
    ):
        if use_cuda and not args.presend_cuda:
            batch = ut.send_data(batch, device, dtype_map={torch.float16: amp_dtype})

        batch_step = batch_idx + 1
        log_step = batch_step % print_freq == 0 or batch_step == num_batches
        update_in_epoch = batch_idx // args.accum_iter
        group_size = min(args.accum_iter, num_batches - update_in_epoch * args.accum_iter)
        need_update = batch_step % args.accum_iter == 0 or batch_step == num_batches
        global_step = epoch * steps_per_epoch + update_in_epoch
        lr = lr_schedule[global_step]
        if need_update:
            ut.update_lr(optimizer.param_groups, lr)

        images, img_mask = mri_data.densify_sparse_image_batch(
            batch["image_values"],
            batch["img_mask"],
            (int(args.get("in_chans", 1)), *args.img_size),
            dtype=amp_dtype,
        )

        sync_context = model.no_sync() if args.distributed and not need_update else nullcontext()
        with sync_context:
            with torch.autocast(device_type=device.type, dtype=amp_dtype, enabled=args.amp):
                loss = model(
                    images,
                    img_mask=img_mask,
                    mask_ratio=args.mask_ratio,
                    pred_mask_ratio=args.pred_mask_ratio,
                    pad_to_multiple=args.pad_to_multiple,
                    with_state=False,
                )

            loss_for_log = loss.detach()
            torch._assert_async(torch.isfinite(loss_for_log), "non-finite loss")

            grad_norm = ut.backward_step(
                loss / group_size,
                optimizer,
                scaler=loss_scaler,
                need_update=need_update,
                max_norm=args.clip_grad,
            )

        if need_update and log_step:
            loss_value = loss_for_log.item()
            grad_norm_value = grad_norm.item()
            metric_logger.update(loss=loss_value, lr=lr, grad=grad_norm_value)
            if log_wandb:
                wandb.log(
                    {
                        "train/loss": loss_value,
                        "train/lr": lr,
                        "train/grad": grad_norm_value,
                    },
                    step=int(1000 * (epoch + batch_step / epoch_num_batches)),
                )

    # gather the stats from all processes
    metric_logger.synchronize_between_processes()
    print("Averaged stats:", metric_logger)
    return {f"train/{k}": meter.global_avg for k, meter in metric_logger.meters.items()}


@torch.inference_mode()
def evaluate(
    args: DictConfig,
    model: nn.Module,
    data_loader: Iterable,
    epoch: int,
    device: torch.device,
    eval_name: str,
):
    model.eval()

    metric_logger = ut.MetricLogger(delimiter="  ")
    header = f"Eval ({eval_name}): [{epoch}]"
    is_master = ut.is_main_process()
    log_wandb = args.wandb and is_master

    epoch_num_batches = len(data_loader)
    if epoch_num_batches <= 0:
        raise ValueError(f"eval loader {eval_name!r} has zero batches")

    print_freq = args.get("print_freq", 100) if not args.debug else 1
    num_batches = epoch_num_batches if not args.debug else 10
    num_batches = min(num_batches, epoch_num_batches)
    eval_seed = int(args.get("eval_seed", args.seed)) + ut.get_rank()
    example_step = random.Random(eval_seed).randint(1, num_batches)
    amp_dtype = getattr(torch, args.amp_dtype)
    use_cuda = device.type == "cuda"
    rng_state = ut.capture_rng_state()
    torch.set_rng_state(torch.Generator().manual_seed(eval_seed).get_state())
    if use_cuda:
        torch.cuda.manual_seed(eval_seed)
    if use_cuda and args.presend_cuda:
        data_loader = ut.pre_send_to_cuda_wrapper(
            data_loader, device, dtype_map={torch.float16: amp_dtype}
        )

    eval_batches = islice(data_loader, num_batches)
    for batch_idx, batch in enumerate(
        metric_logger.log_every(eval_batches, print_freq, header, total_steps=num_batches)
    ):
        if use_cuda and not args.presend_cuda:
            batch = ut.send_data(batch, device, dtype_map={torch.float16: amp_dtype})

        batch_step = batch_idx + 1

        images, img_mask = mri_data.densify_sparse_image_batch(
            batch["image_values"],
            batch["img_mask"],
            (int(args.get("in_chans", 1)), *args.img_size),
            dtype=amp_dtype,
        )

        with torch.autocast(device_type=device.type, dtype=amp_dtype, enabled=args.amp):
            loss, state = model(
                images,
                img_mask=img_mask,
                mask_ratio=args.mask_ratio,
                pred_mask_ratio=args.pred_mask_ratio,
                pad_to_multiple=args.pad_to_multiple,
            )

        loss_value = loss.detach().float().item()
        finite = torch.tensor(int(math.isfinite(loss_value)), dtype=torch.int32, device=device)
        if args.distributed:
            torch.distributed.all_reduce(finite, op=torch.distributed.ReduceOp.MIN)
        if not finite.item():
            raise RuntimeError("non-finite validation loss detected")
        metric_logger.meters["loss"].update(loss_value, n=int(batch["img_mask"].shape[0]))

        if is_master and batch_step == example_step:
            example_batch = {"image": images[:1], "img_mask": img_mask[:1]}
            if "meta" in batch:
                example_batch["meta"] = batch["meta"][:1]
            example_state = {
                "pred_images": state["pred_images"][:1],
                "pred_mask": state["pred_mask"][:1],
            }
            example_data = {
                "batch": ut.send_data(example_batch, "cpu"),
                "state": ut.send_data(example_state, "cpu"),
            }

    # gather the stats from all processes
    metric_logger.synchronize_between_processes()
    print(f"Averaged stats ({eval_name}):", metric_logger)
    stats = {f"eval/{eval_name}/{k}": meter.global_avg for k, meter in metric_logger.meters.items()}

    plots = {}
    if is_master:
        print(f"Making plots ({eval_name}): example={example_step}")
        plots = make_plots(args, **example_data)
        plots = {f"eval/{eval_name}/{k}": img for k, img in plots.items()}

    if log_wandb:
        wandb.log(stats, step=1000 * (epoch + 1))
        wandb.log(
            {k: wandb.Image(img, caption=f"example={example_step}") for k, img in plots.items()},
            step=1000 * (epoch + 1),
        )
    ut.restore_rng_state(rng_state)
    return stats, plots


def make_plots(
    args: DictConfig,
    batch: dict[str, Tensor],
    state: dict[str, Tensor],
) -> dict[str, Image.Image]:
    fig_kwargs = args.get("fig_kwargs", {})

    images = batch["image"]
    img_mask = batch.get("img_mask")
    if img_mask is not None:
        img_mask = img_mask.expand_as(images)

    raw_mean, raw_std = vis.raw_stats_from_batch(batch)

    plots = {}
    mask_pred_fig = vis.plot_mask_pred(
        target=images,
        pred=state["pred_images"],
        pred_mask=state["pred_mask"],
        img_mask=img_mask,
        patch_size=args.patch_size,
        raw_mean=raw_mean,
        raw_std=raw_std,
        **ut.filter_kwargs(vis.plot_mask_pred, fig_kwargs),
    )
    plots["mask_pred"] = vis.fig2pil(mask_pred_fig)
    plt.close(mask_pred_fig)

    return plots


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--cfg-path", type=str, default=None)
    parser.add_argument("--overrides", type=str, default=None, nargs="+")
    args = parser.parse_args()
    cfg = OmegaConf.load(DEFAULT_CONFIG)
    if args.cfg_path:
        cfg = OmegaConf.unsafe_merge(cfg, OmegaConf.load(args.cfg_path))
    if args.overrides:
        cfg = OmegaConf.unsafe_merge(cfg, OmegaConf.from_dotlist(args.overrides))
    main(cfg)
