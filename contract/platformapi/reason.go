package platformapi

// The reasons of a refusal (ErrorModel.Reason) that a client branches on.
const (
	// ReasonExperimentClosed refuses what only an open or running platform experiment does.
	ReasonExperimentClosed = "experiment_closed"
	// ReasonSimilarHypotheses refuses a hypothesis that reads like others already registered,
	// which the answer lists.
	ReasonSimilarHypotheses = "similar_hypotheses"
	// ReasonQuotaNotGranted refuses the quota of an agent whose platform experiment has not
	// started.
	ReasonQuotaNotGranted = "quota_not_granted"
	// ReasonPlannerHasNoQuota refuses the quota of a draft's planner, which holds none.
	ReasonPlannerHasNoQuota = "planner_has_no_quota"
	// ReasonCursorGone refuses a watch or a read of changes from an after this platform holds no
	// changes past: one it never issued, or one whose changes are pruned. The client reads state
	// again and watches from head.
	ReasonCursorGone = "cursor_gone"
)
