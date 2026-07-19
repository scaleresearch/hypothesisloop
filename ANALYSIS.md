# Repo Analysis — One Finding

## Bug: CPU-only platform experiments are instantly evicted for "quota_exhaustion"

**Where:** `controlplane/services/controller/reconcile.go:141-142` (`checkQuotaExhaustion`),
with the mirror comparison at `controlplane/services/controller/checks.go:159-161`
(`researcherHasCapacity`).

```go
guaranteedExhausted := (aq.UsedGuaranteedAccH + deltaGuaranteed) >= aq.GuaranteedAcceleratorHours*0.99
burstExhausted      := (aq.UsedBurstAccH + deltaBurst)           >= aq.BurstAcceleratorHours*0.99
```

### Why it's wrong

The exhaustion test only ever looks at the **accelerator-hours** dimension. The system now
supports CPU-only jobs and CPU-only platform experiments (see the "Class B" CPU/RAM/storage
work in `admission.go`, `Footprint()`, and `settlement.go`). A platform experiment that tracks
only CPU has `BudgetAcceleratorHours == 0`, so `Start()` allocates
`GuaranteedAcceleratorHours == 0` and `BurstAcceleratorHours == 0`
(`platform_experiments_lifecycle.go`, `AllocateQuota(0, …) → 0,0`).

For such an experiment, every running job is CPU-only, so:

- `exp.EstimatedCostAccH == 0` and observed accelerator cost `== 0`, hence `deltaGuaranteed == 0`
- `aq.UsedGuaranteedAccH == 0`

The comparison becomes:

```
(0 + 0) >= 0 * 0.99   →   0 >= 0   →   true
```

`guaranteedExhausted` (and `burstExhausted`) evaluate to **true on the very first reconcile
tick**, before the job has consumed anything. `checkQuotaExhaustion` then:

1. Evicts every RUNNING guaranteed/burst job for that agent with reason
   `quota_exhaustion` (`reconcile.go:148-186`), and
2. Rejects all QUEUED/SUBMITTED jobs for the same agent (`cancelPreRun`, `reconcile.go:190-230`).

So a perfectly healthy CPU-only workload is killed and mislabeled as out-of-budget, and new
submissions are refused — a full denial of service for any accelerator-free platform experiment.

The same `>= budget*0.99` shape flips `researcherHasCapacity` (checks.go:159-161) to `false`
in that case too, so even the overrun-eviction reprieve path treats the agent as having no
capacity.

### Failure scenario (concrete)

1. Operator creates a platform experiment with `budget_cpu_core_hours > 0` and
   `budget_accelerator_hours == 0`.
2. An agent submits a CPU-only job (`accelerator_count == 0`, positive `job.cpu`) — accepted by
   `ValidateExperiment`, admitted by the CPU-capacity path in `loop_tick.go`, reaches RUNNING.
3. On the next controller reconcile tick, `checkQuotaExhaustion` computes `0 >= 0 == true` and
   evicts the job with `eviction_reason = quota_exhaustion`; queued siblings are rejected.

Net effect: CPU-only experiments can never keep a job running.

### Fix direction

Guard the per-tier test so a zero (untracked) accelerator budget is not treated as "already
exhausted," and evaluate exhaustion per resource dimension actually tracked rather than only
accelerator-hours. Minimal guard:

```go
guaranteedExhausted := aq.GuaranteedAcceleratorHours > 0 &&
    (aq.UsedGuaranteedAccH + deltaGuaranteed) >= aq.GuaranteedAcceleratorHours*0.99
burstExhausted := aq.BurstAcceleratorHours > 0 &&
    (aq.UsedBurstAccH + deltaBurst) >= aq.BurstAcceleratorHours*0.99
```

Apply the symmetric `> 0` guard to `researcherHasCapacity`. A complete fix would also add the
CPU-core-hours dimension to the exhaustion check so CPU budgets are actually enforced (today
nothing evicts on CPU-budget exhaustion at all — the flip side of the same accelerator-only
assumption), but the `> 0` guard is what stops the immediate mis-eviction.
