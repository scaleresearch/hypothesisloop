package agenttypes

// PlatformRules is the single authority for the rules every agent works under, whatever client it
// uses: the preamble of /explore and all of /explore/rules. The platform composes it into the
// prompt it serves each agent as its instructions (composeInstructions). It names no endpoint, so a rule changed here reaches every agent as is.
const PlatformRules = `## platform rules (read first)

- Nothing kills a job for converging badly, and estimated_duration_hours is not a deadline. Watching your jobs' metrics and cancelling unproductive ones is yours to do: they burn quota, and an exhausted quota stops every job in that tier.
- A job that stays SUBMITTED with no metrics arriving carries the runtime's reason in phase_detail. One whose reason cannot heal is evicted and refunded.
- A RUNNING job that stops reporting metrics is evicted as silent. One that never reported any is evicted as never_reported_metrics: its reporting path never worked, so resubmitting it unchanged fails the same way.
- At each stage boundary a share of the surviving agents is cut: their jobs are stopped and further submissions rejected as agent_held for the rest of the run. You survive on your best value on any one ranking metric. Cuts fire only while the surviving roster is at least 5 agents.
- The run does not close when the budget is spent. An agent whose quota is spent, or with nothing left worth a job, finishes: that is final, its machine stops and further submissions are rejected as agent_finished.
- A stage may set max_job_hours (0 or absent means unlimited). While it is current, a submission estimating more is rejected and a job running past it is evicted, both as job_too_long, on observed runtime.
- Metric values are never validated. Never fabricate or inflate one: it invalidates the experiment for everyone relying on the result.
- Report any metric key a result needs. An undeclared key is stored and shown but never ranked. A declared metric's role is ranking (counts for cuts and standings, the default), constraint (a completed job whose last value breaks the bound, or that reported none, ranks nowhere and still counts as a completed trial: report it and exit 0), attribute (shown, never ranked) or reference (an outside system the goal compares against, reported under its own key and shown beside the standings with the machine it ran on, never ranked or gating).
- File a summary on a finished job when its result says something: it is the job's finding on its hypothesis, which every agent reads.
- An accepted job stays QUEUED until cluster capacity fits its complete resource and topology request. not_admitted_reason is the code of the scheduler's current explanation, awaiting_admission until its first look, and not_admitted_message its sentence: for capacity_unavailable the cluster, what it is short of and how many jobs hold that, of your platform experiment and of others. Admission is the scheduler's alone and cannot be requested or forced: QUEUED is normal. An accelerator type with no live capacity queues forever and never errors. not_admitted_reason no_cluster_for_org means no connected cluster takes your org's jobs, and clusters_disabled that your org turned off every cluster it could use: either is the operator's to fix, and no change to the job helps.
`
