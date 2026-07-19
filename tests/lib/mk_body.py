"""Body builders shared by tests/lib/api.sh. Invoked as:
  python3 mk_body.py hyp AGENT PE_ID
  python3 mk_body.py submit JOB_ID AGENT PE_ID HOURS JOB_FILE HYP_ID TIER [GPU_TYPE] [GPU_COUNT] \\
                            [NUM_NODES] [ENV_JSON] [PROJECT_ID] [THEORY] [OBJECTIVE] [HYP_TEXT] \\
                            [JOB_OVERRIDE_JSON]

JOB_OVERRIDE_JSON is a JSON object merged directly over the loaded job.yaml (after the
gpu_type/gpu_count/num_nodes overrides above) — the one escape hatch every scenario needs for
fields job.yaml doesn't have a dedicated CLI arg for (cpu, storage, memory, an intentionally
omitted/invalid field, gpu_count=0 for a CPU-only job, ...), so no scenario has to hand-roll
its own Python job-body literal just to override one or two fields.
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
     gpu_type, gpu_count, num_nodes, env_json, project_id, theory, objective,
     job_override_json) = (sys.argv[2:17] + [""] * 15)[:15]

    with open(job_file) as f:
        job = yaml.safe_load(f)

    if env_json:
        job["env"] = {**job.get("env", {}), **json.loads(env_json)}

    if gpu_type:
        # Pin to exactly one type (drop acceptable_gpu_types tolerance) so a scenario can
        # force capacity contention against that type's known cluster_gpus count.
        job["gpu_type"] = gpu_type
        job.pop("acceptable_gpu_types", None)
    if gpu_count:
        job["gpu_count"] = int(gpu_count)
    if num_nodes:
        job["num_nodes"] = int(num_nodes)
        # The local dev cluster has exactly one node per GPU type (localdev/add-fake-nodes.sh),
        # so a hard distinct-hosts requirement would make any num_nodes>1 job unschedulable.
        job["topology"] = {"spread_across_hosts": False}
    if job_override_json:
        overrides = json.loads(job_override_json)
        for key, value in overrides.items():
            if value is None:
                job.pop(key, None)  # explicit null means "omit this field entirely"
            else:
                job[key] = value

    metadata = {
        "agent_id": agent,
        "platform_experiment_id": pe_id,
        "project_id": project_id or "e2e",
        "hypothesis_id": hyp_id,
        "theory": theory or "e2e scenario coverage",
        "objective": objective or "maximize val_accuracy",
        "estimated_duration_hours": float(hours),
        "code_ref": "git://openresearch@main",
    }
    if tier:
        metadata["capacity_tier"] = tier
    print(json.dumps({"id": job_id, "metadata": metadata, "job": job}))

else:
    sys.exit(f"unknown kind: {kind}")
