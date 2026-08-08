"""The FOMO26 task zips, read straight off disk.

Upstream's `fomo_tune/datasets.py` streams the same zips into a
`datasets.Dataset` with the `Nifti()` feature type. That feature only exists in
very recent `datasets` releases; the tt-metal python env this port runs in
ships `datasets` 2.21, and pinning a newer one would drag pyarrow/pandas into a
job image for no gain -- nothing here needs Arrow, a schema, or streaming, only
"give me each subject's label and t1w volume in a stable order".

So this reads an already-unpacked copy of the archives off a node-local path
(`seed/fetch_data.sh` puts one there once, and job specs bind-mount it via
`host_mounts`) -- no job ever downloads or unzips 3.3GB on start. The subject
ordering, the member paths and the label parsing are all taken verbatim from
`datasets.py`'s `subject_ids`/`_fomo_task3_generator`/`_fomo_task5_generator`,
so the row order this yields is the one the upstream KFold protocol splits.
Paths rather than in-memory volumes because `nib.load` is lazy: 494 subjects of
dense float volumes do not fit in RAM at once, and the feature pass only ever
needs one at a time.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

TASK_SPECS = {
    "task3": {
        "root": "Task_3",
        "session": "ses-01",
        "image": "t1w",
        "url": "https://sid.erda.dk/share_redirect/fmeuvo1EdF/Task_3.zip",
    },
    "task5": {
        "root": "Task_5",
        "session": "ses_01",
        "image": "t1",
        "url": "https://huggingface.co/datasets/medarc/smri-fm/resolve/main/fomo_eval/Task_5.zip",
    },
}


@dataclass(frozen=True)
class Subject:
    subject: str
    label: int  # task 3: age in years; task 5: 0/1 polymicrogyria
    t1w_path: Path


def load_task(task: str, data_root: Path) -> list[Subject]:
    """`data_root` is the directory the task archives were unpacked into, i.e.
    the one holding `Task_3/`/`Task_5/`. See `seed/fetch_data.sh`."""
    spec = TASK_SPECS[task]
    root, session, image = spec["root"], spec["session"], spec["image"]
    preprocessed = data_root / root / "preprocessed"
    if not preprocessed.is_dir():
        raise FileNotFoundError(
            f"{preprocessed} does not exist. This path is expected to be an already-unpacked, "
            "node-local dataset bind-mounted into the job (seed/job.yaml's host_mounts); a job "
            "does not fetch it. Run seed/fetch_data.sh on the node once if it is genuinely missing."
        )

    rows = []
    for subject_dir in sorted(preprocessed.iterdir()):
        sub = subject_dir.name
        label = int((data_root / root / "labels" / sub / session / "labels.txt").read_text().strip())
        rows.append(Subject(subject=sub, label=label, t1w_path=subject_dir / session / f"{image}.nii.gz"))
    return rows
