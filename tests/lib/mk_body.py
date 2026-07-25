"""Body builders shared by tests/lib/api.sh. Invoked as:
  python3 mk_body.py hyp AGENT PE_ID
  python3 mk_body.py submit JOB_ID AGENT PE_ID HOURS JOB_FILE HYP_ID TIER [ACCELERATOR_TYPE] [ACCELERATOR_COUNT] \\
                            [NUM_NODES] [ENV_JSON] [PROJECT_ID] [THEORY] [OBJECTIVE] [HYP_TEXT] \\
                            [JOB_OVERRIDE_JSON]

JOB_OVERRIDE_JSON is a JSON object merged over the loaded job.yaml (after the
accelerator_type/accelerator_count/num_nodes overrides above) — an escape hatch for fields
job.yaml has no dedicated CLI arg for.
"""
import json
import sys

import yaml

kind = sys.argv[1]

if kind == "hyp":
    agent, pe_id, text = (sys.argv[2:5] + [""] * 3)[:3]
    print(json.dumps({
        "agent_id": agent,
        "platform_experiment_id": pe_id,
        "text": text or f"e2e run for {agent}",
    }))

elif kind == "submit":
    (job_id, agent, pe_id, hours, job_file, hyp_id, tier,
     accelerator_type, accelerator_count, num_nodes, env_json, project_id, theory, objective,
     job_override_json) = (sys.argv[2:17] + [""] * 15)[:15]

    with open(job_file) as f:
        job = yaml.safe_load(f)

    if env_json:
        job["env"] = {**job.get("env", {}), **json.loads(env_json)}

    if accelerator_type:
        # Pin the fixture to one type: it becomes the requested type, and any alternatives the
        # fixture listed are dropped so the run cannot silently land on different hardware.
        job["accelerator_type"] = accelerator_type
        job.pop("acceptable_accelerator_types", None)
    if accelerator_count:
        job["accelerator_count"] = int(accelerator_count)
    if num_nodes:
        job["num_nodes"] = int(num_nodes)
    if job_override_json:
        overrides = json.loads(job_override_json)
        for key, value in overrides.items():
            if value is None:
                job.pop(key, None)
            else:
                job[key] = value

    metadata = {
        "agent_id": agent,
        "platform_experiment_id": pe_id,
        "project_id": project_id or "e2e",
        "hypothesis_id": hyp_id,
        "hypothesis": f"e2e run for {agent}",
        "theory": theory or "e2e scenario coverage",
        "objective": objective or "maximize val_accuracy",
        "estimated_duration_hours": float(hours),
        # Fixture value matching admission.go's codeRefPattern (<url>@<40-hex-sha>); doesn't
        # resolve to a real commit.
        "code_ref": "git://hypothesisloop@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        # Required by schema, unvalidated at admission time — no real provenance to reference.
        "config_hash": "",
        "data_ref": "",
    }
    if tier:
        metadata["capacity_tier"] = tier
    print(json.dumps({"id": job_id, "metadata": metadata, "job": job}))

else:
    sys.exit(f"unknown kind: {kind}")
