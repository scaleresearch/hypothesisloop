# Provisioning a real GPU node on vast.ai and connecting it to HypothesisLoop's control plane

What to do when you need to test bare-metal or k3s cluster-agent code against **real** GPU
hardware (NVIDIA or otherwise) instead of simulated/local nodes. Confirmed working end-to-end
against real NVIDIA RTX 4070 Ti / RTX 4090 hardware.

## 1. Prerequisites

- A vast.ai account + API key (`vastai set api-key <key>`). Install the CLI:
  `pip install vastai` (use a venv if the host's Python is externally-managed: PEP 668).
- An SSH keypair dedicated to this, registered with vast.ai:
  ```
  ssh-keygen -t ed25519 -f ~/.ssh/vastai_hypothesisloop -N '' -C "hypothesisloop-e2e"
  vastai attach-ssh <instance-id> "$(cat ~/.ssh/vastai_hypothesisloop.pub)"   # per-instance
  # or upload once and select at instance creation with --ssh-key-id
  ```

## 2. Why you MUST use the KVM VM template, not a plain `--image`

Standard vast.ai "on-demand" instances launched via `--image` are themselves **unprivileged
Docker containers** on the vast.ai host — no `SYS_ADMIN`/`NET_ADMIN`, `unshare()`/`clone()`
blocked by seccomp. This breaks nested Docker/Podman entirely (needed for `bare-agent`'s
job-container execution model and for k3s). Confirmed broken across 4+ independent hosts —
this is a platform-wide restriction, not a host fluke. Do not waste time debugging
`--iptables=false`, fuse-overlayfs, vfs storage drivers, etc. — none of it works.

The fix: rent an offer with vast.ai's own **KVM VM template**, which gives a real virtual
machine with full root/kernel access:

```bash
# Find the template (id 153858 as of writing, but look it up fresh — don't hardcode):
vastai search templates --raw | python3 -c "
import json,sys
for t in json.load(sys.stdin):
    if t.get('image') == 'docker.io/vastai/kvm':
        print(t['id'], t['hash_id'], t['name'], t['recent_create_date'])
"
```

Search for offers with `vms_enabled=true` and rent with `--template_hash <that hash>`:

```bash
vastai search offers --raw 'vms_enabled=true num_gpus=1 gpu_ram>=8 rentable=true' \
  | python3 -c "import json,sys; d=json.load(sys.stdin); d.sort(key=lambda o:o['dph_total']); [print(o['id'],o['gpu_name'],o['dph_total'],o['geolocation']) for o in d[:10]]"

vastai create instance <offer_id> --template_hash <kvm_template_hash> --disk 40
```

## 3. Boot is flaky — budget for retries

Real observed failure modes, in order of frequency:
- Instance shows `running` but SSH (both proxy `sshN.vast.ai:PORT` and the direct
  `ports["22/tcp"]` mapping) refuses connections for 10+ minutes on some hosts, even though
  `vastai logs <id>` shows the VM fully booted internally (systemd, cloud-init, sshd all up,
  your key correctly authorized). This is a broken host-side port-forward, not something you
  can fix. **Destroy and rent a different host** rather than waiting it out.
- `vastai create instance` returns `"success": false` with `intended_status: "stopped"` — the
  instance was created but never started. `vastai start instance <id>` — sometimes queues with
  "Required resources are currently unavailable" and never actually starts. Destroy, try again.
- `vastai logs <id>` shows `"GPU error, unable to start instance"` — bad host, destroy and
  retry on a different offer.

Practical approach: poll `vastai show instance <id> --raw` for `actual_status == "running"`,
then try an actual `ssh ... echo alive` (not just a raw TCP connect check — those can give
false negatives/positives). Give each host ~3-5 minutes; if still unreachable, destroy and
rent a different offer/host rather than debugging further. This is expected, not exceptional.

## 4. Verify the VM is real and GPU-capable before doing anything else

```bash
ssh -i ~/.ssh/vastai_hypothesisloop -p <port> root@<ip> '
  systemd-detect-virt          # must print "kvm", not e.g. "docker"/"lxc"
  nvidia-smi -L                # confirms the GPU is visible
  docker run --rm hello-world  # confirms nested Docker actually works (the whole point)
'
```

## 5. Install nvidia-container-toolkit and wire it into Docker

Raw `--device /dev/nvidia0` passthrough is NOT enough — CUDA images need the driver's
userspace libraries (`libnvidia-ml.so`, `nvidia-smi`, etc.), which only `--gpus`/the toolkit's
runtime hook injects. Without this, `nvidia-smi` inside any container says "not found" even
though the device node is there.

```bash
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-container-toolkit
nvidia-ctk runtime configure --runtime=docker
systemctl restart docker
docker run --rm --gpus all nvidia/cuda:12.4.1-base-ubuntu22.04 nvidia-smi -L   # must work now
```

## 6. Reach your local control plane from the rented node

The control plane normally runs locally (`make controlplane-up`, or
`bash controlplane/infra/podman.sh start` to resume an existing stopped pod). The rented node
needs outbound URLs that actually reach it — use Cloudflare Quick Tunnels:

```bash
bash localdev/tunnels/tunnels.sh up      # prints URLs for :8081-8084
bash localdev/tunnels/tunnels.sh status  # re-check anytime; URLs re-randomize on restart
```

## 7A. Bare-metal agent path

```bash
GOOS=linux GOARCH=amd64 go build -o /tmp/bare-agent ./runtime/bare-metal/cmd/bare-agent
scp -i ~/.ssh/vastai_hypothesisloop -P <port> /tmp/bare-agent controlplane/settings/hypothesisloop.yaml root@<ip>:/root/
ssh ... 'CLUSTER_NAME=my-gpu-node \
  CONTROLPLANE_URL=<scheduler tunnel URL> \
  REGISTRY_URL=<registry tunnel URL> \
  HYPOTHESISLOOP_CONFIG=/root/hypothesisloop.yaml \
  NODE_NAME=my-gpu-node \
  nohup /root/bare-agent > /root/bare-agent.log 2>&1 &'
```

Verify registration: `curl <scheduler tunnel>/internal/clusters` and
`curl <quota tunnel>/resource-catalog/capacity`.

## 7B. k3s / cluster-agent path

Use `localdev/k3s-nvidia/install.sh` (mirrors `localdev/k3s-tenstorrent-qb2/`) — installs
nvidia-container-toolkit, k3s (`--docker`), the NVIDIA device plugin, GPU Feature Discovery,
and the cluster-agent bundle in one shot. Requires `CONTROLPLANE_URL`/`REGISTRY_URL`/
`METRICS_URL` env vars pointed at your tunnels. See that script's header comment for why it
manually applies GPU Feature Discovery's node labels (no Node Feature Discovery installed).

## 8. Pricing gotcha: the accelerator type must be in `hypothesisloop.yaml`

`nvidia.com/gpu.product=<NAME>` must have an `acch_rate` entry in
`controlplane/settings/hypothesisloop.yaml`'s `accelerator_types`, or jobs requesting that
hardware can never be admitted. Find out what your rented GPU will report with
`nvidia-smi --query-gpu=name --format=csv,noheader`. Matching against the priced catalog is
case-insensitive (`domain.AcceleratorType.MatchesLabels`), so don't worry about exact casing —
different discovery mechanisms (this platform's own NVML probe vs. real NVIDIA GPU Feature
Discovery) report the same hardware with different casing, and that's handled automatically.
After editing the yaml, restart the control-service container to pick it up:
`podman restart hypothesisloop-control-service`.

## 9. Submitting a real job

See `tests/lib/api.sh`'s `register_agent`/`create_platform_experiment`/`signup_and_start`/
`submit_job_ext`, and `tests/scenarios/nvidia-hardware.sh` for a full worked example (bare-node
+ optional k3s leg, positive completion, negative-admission, mid-run eviction). Force-admit
onto your specific node while testing (bypasses the normal capacity-based scheduling race):
```bash
curl -X POST <scheduler tunnel>/experiments/<job_id>/admit \
  -H 'Content-Type: application/json' -d '{"cluster_name": "my-gpu-node"}'
```

## 10. Teardown

Always destroy when done — billed hourly:
```bash
vastai destroy instance <id> -y
vastai show instances --raw   # confirm empty
```

## Reusable test assets already in this repo

- `tests/workloads/nvidia/{Dockerfile.train,train.py,job.yaml}` — real CUDA fp16 matmul
  benchmark workload (same metric contract as `tests/workloads/tenstorrent/`), supports an
  optional repeat-forever mode (`HYPOTHESISLOOP_REPEAT_FOREVER=1`) for eviction/long-run tests.
- `tests/scenarios/nvidia-hardware.sh` — hardware-gated (`RUN_HARDWARE_TESTS=1`), covers both
  the bare-node and (optional) k3s legs.
- `localdev/k3s-nvidia/{install,status,destroy,run-e2e}.sh` — spin up/tear down/test a real
  k3s+NVIDIA cluster on a host like this.

## Known limitations at time of writing

- Single-GPU testing only — `accelerator_count > 1` on one node not yet exercised.
- Only tested against consumer GPUs (RTX 4070 Ti, RTX 4090, RTX 3080) — not A100/H100/L40,
  which are the ones actually priced by default in `hypothesisloop.yaml`.
- No load/concurrent-job or long-duration (hours) real training workload testing yet.
