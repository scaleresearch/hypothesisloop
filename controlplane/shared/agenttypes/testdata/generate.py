"""Writes the built-in prompts (../builtin/<id>.md) and the golden prompts the platform must serve
(golden/<id>.txt) from the experimentator's own composition, core.system_prompt, so that both say
exactly what the harness said before the prompts moved into the database.

Run from the repository root with the experimentator's environment:
    uv run --project agents/experimentator python controlplane/shared/agenttypes/testdata/generate.py
"""
import json
import shutil
import tempfile
from pathlib import Path
from types import SimpleNamespace

from hypothesisloop_agent import core

HERE = Path(__file__).parent
FLAVORS = ["generalist", "hyperparameter-search", "architecture-search", "data-search"]
# id: (flavor, role) as the harness was configured for it.
BUILTINS = {**{f: (f, "competitor") for f in FLAVORS},
            "baseline": ("generalist", "baseline"), "planner": ("planner", "planner"), "coordinator": ("coordinator", "")}
ASSIGNMENT = json.loads((HERE / "assignment.json").read_text())
# The harness reads hyperparameters from the platform's answer, whose objects hold their keys sorted.
ASSIGNMENT["hyperparameters"] = dict(sorted(ASSIGNMENT["hyperparameters"].items()))
HYPERPARAMETERS = object()


def compose(builtin: str, placeholders: bool) -> str:
    flavor, role = BUILTINS[builtin]
    if placeholders:
        cfg = SimpleNamespace(flavor=flavor, role=role, platform_experiment_id="{{platform_experiment_id}}",
                              agent_id="{{agent_id}}", hyperparameters=HYPERPARAMETERS)
        if flavor in FLAVORS and role == "competitor":
            cfg.flavor = "{{agent_type}}"
        return core.system_prompt(cfg, "{{rules}}", "{{hl_commands}}", "{{repository_url}}", "{{workdir}}")
    cfg = SimpleNamespace(flavor=flavor, role=role, platform_experiment_id=ASSIGNMENT["platform_experiment_id"],
                          agent_id=ASSIGNMENT["agent_id"], hyperparameters=ASSIGNMENT["hyperparameters"])
    return core.system_prompt(cfg, ASSIGNMENT["rules"], "{{hl_commands}}", ASSIGNMENT["repository_url"], "{{workdir}}")


def main() -> None:
    prompts = Path(core._PROMPTS_DIR)
    with tempfile.TemporaryDirectory() as tmp:
        # The brief of a flavor is read by its name, which the placeholder stands in for.
        staged = Path(tmp) / "prompts"
        shutil.copytree(prompts, staged)
        dumps = json.dumps
        core.json = SimpleNamespace(dumps=lambda v: "{{hyperparameters}}" if v is HYPERPARAMETERS else dumps(v))
        for builtin, (flavor, role) in BUILTINS.items():
            if flavor in FLAVORS and role == "competitor":
                shutil.copy(prompts / "flavors" / f"{flavor}.md", staged / "flavors" / "{{agent_type}}.md")
            core._PROMPTS_DIR = staged
            (HERE.parent / "builtin" / f"{builtin}.md").write_text(compose(builtin, True))
            core._PROMPTS_DIR = prompts
            (HERE / "golden" / f"{builtin}.txt").write_text(compose(builtin, False))


if __name__ == "__main__":
    main()
