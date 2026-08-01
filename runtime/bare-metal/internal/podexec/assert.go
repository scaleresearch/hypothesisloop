package podexec

import "github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"

// Compile-time check that Executor satisfies the shared cluster-side contract (see
// runtime/shared/agentexec) — the same method set runtime/k8s/internal/k8sexec.JobWorkloadClient
// structurally implements.
var _ agentexec.Executor = (*Executor)(nil)
