import os
import shutil
import tempfile
import zipfile
from collections.abc import Generator
from contextlib import contextmanager
from pathlib import Path

import fsspec
from datasets import Dataset, Features, Nifti, Value

FOMO_EVAL_BASE_URL = os.getenv(
    "FOMO_EVAL_BASE_URL",
    "https://sid.erda.dk/share_redirect/fmeuvo1EdF",
)
FOMO_EVAL_TASK5_URL = os.getenv(
    "FOMO_EVAL_TASK5_URL",
    "https://huggingface.co/datasets/medarc/smri-fm/resolve/main/fomo_eval/Task_5.zip",
)


@contextmanager
def open_zip(url: str) -> Generator[zipfile.ZipFile, None, None]:
    """Open a task zip, copying a remote url to a temp file first."""
    with tempfile.TemporaryDirectory() as tmp:
        local = Path(url)
        if not local.exists():
            local = Path(tmp) / "task.zip"
            with fsspec.open(url) as src, local.open("wb") as dst:
                shutil.copyfileobj(src, dst)
        with zipfile.ZipFile(local) as zf:
            yield zf


def subject_ids(zf: zipfile.ZipFile) -> list[str]:
    return sorted({name.split("/")[2] for name in zf.namelist() if name.endswith(".nii.gz")})


# ---- Task 1: acute infarct (classification; positives also carry a lesion mask) --------


def load_fomo_task1() -> Dataset:
    # No 4th modality: it is swi on 16 subjects and t2s on the other 5.
    suffixes = ("adc", "dwi_b1000", "flair")
    features = Features(
        {
            "subject": Value("string"),
            "label": Value("int32"),
            **{suffix: Nifti() for suffix in suffixes},
        }
    )
    dataset = Dataset.from_generator(
        _fomo_task1_generator,
        features=features,
        gen_kwargs={"suffixes": suffixes},
        writer_batch_size=16,
    )
    return dataset


def _fomo_task1_generator(suffixes: tuple[str, ...]):
    url = f"{FOMO_EVAL_BASE_URL}/Task_1.zip"
    with open_zip(url) as zf:
        for sub in subject_ids(zf):
            label = int(zf.read(f"Task_1/labels/{sub}/ses-01/label.txt").strip())
            sample = {"subject": sub, "label": label}
            for suffix in suffixes:
                name = f"Task_1/preprocessed/{sub}/ses-01/{suffix}.nii.gz"
                sample[suffix] = {"path": None, "bytes": zf.read(name)}
            yield sample


# ---- Task 2: meningioma segmentation ---------------------------------------------------


def load_fomo_task2() -> Dataset:
    # No 4th modality: it is t2s on 15 subjects and swi on the other 8.
    suffixes = ("dwi_b1000", "flair")
    features = Features(
        {
            "subject": Value("string"),
            **{suffix: Nifti() for suffix in suffixes},
            "seg": Nifti(),
        }
    )
    dataset = Dataset.from_generator(
        _fomo_task2_generator,
        features=features,
        gen_kwargs={"suffixes": suffixes},
        writer_batch_size=16,
    )
    return dataset


def _fomo_task2_generator(suffixes: tuple[str, ...]):
    url = f"{FOMO_EVAL_BASE_URL}/Task_2.zip"
    with open_zip(url) as zf:
        for sub in subject_ids(zf):
            sample = {"subject": sub}
            for suffix in suffixes:
                name = f"Task_2/preprocessed/{sub}/ses-01/{suffix}.nii.gz"
                sample[suffix] = {"path": None, "bytes": zf.read(name)}
            # Seg is on the image grid (shapes match) but its affine differs by up to 0.03mm.
            name = f"Task_2/labels/{sub}/ses-01/seg.nii.gz"
            sample["seg"] = {"path": None, "bytes": zf.read(name)}
            yield sample


# ---- Task 3: brain age regression ------------------------------------------------------


def load_fomo_task3() -> Dataset:
    features = Features(
        {
            "subject": Value("string"),
            "age": Value("int32"),
            "t1w": Nifti(),
        }
    )
    dataset = Dataset.from_generator(
        _fomo_task3_generator,
        features=features,
        writer_batch_size=16,
    )
    return dataset


def _fomo_task3_generator():
    url = f"{FOMO_EVAL_BASE_URL}/Task_3.zip"
    with open_zip(url) as zf:
        for sub in subject_ids(zf):
            age = int(zf.read(f"Task_3/labels/{sub}/ses-01/labels.txt").strip())
            image_gz = zf.read(f"Task_3/preprocessed/{sub}/ses-01/t1w.nii.gz")
            sample = {
                "subject": sub,
                "age": age,
                "t1w": {"path": None, "bytes": image_gz},
            }
            yield sample


# ---- Task 4: trigeminal nerve/vessel segmentation --------------------------------------


def load_fomo_task4() -> Dataset:
    # Volumes are uncropped 0.5mm near-iso, ~360x512x512; crop before feeding a model.
    features = Features(
        {
            "subject": Value("string"),
            "t2w": Nifti(),
            "seg": Nifti(),
        }
    )
    dataset = Dataset.from_generator(
        _fomo_task4_generator,
        features=features,
        writer_batch_size=16,
    )
    return dataset


def _fomo_task4_generator():
    url = f"{FOMO_EVAL_BASE_URL}/Task_4.zip"
    with open_zip(url) as zf:
        for sub in subject_ids(zf):
            image_gz = zf.read(f"Task_4/preprocessed/{sub}/ses-01/t2w.nii.gz")
            seg_gz = zf.read(f"Task_4/labels/{sub}/ses-01/seg.nii.gz")
            sample = {
                "subject": sub,
                "t2w": {"path": None, "bytes": image_gz},
                "seg": {"path": None, "bytes": seg_gz},
            }
            yield sample


# ---- Task 5: polymicrogyria classification ---------------------------------------------


def load_fomo_task5() -> Dataset:
    features = Features(
        {
            "subject": Value("string"),
            "label": Value("int32"),
            "t1w": Nifti(),
        }
    )
    dataset = Dataset.from_generator(
        _fomo_task5_generator,
        features=features,
        writer_batch_size=16,
    )
    return dataset


def _fomo_task5_generator():
    with open_zip(FOMO_EVAL_TASK5_URL) as zf:
        for sub in subject_ids(zf):
            label = int(zf.read(f"Task_5/labels/{sub}/ses_01/labels.txt").strip())
            image_gz = zf.read(f"Task_5/preprocessed/{sub}/ses_01/t1.nii.gz")
            sample = {
                "subject": sub,
                "label": label,
                "t1w": {"path": None, "bytes": image_gz},
            }
            yield sample
