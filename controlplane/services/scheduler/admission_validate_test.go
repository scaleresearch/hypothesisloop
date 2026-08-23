package scheduler

import (
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ValidateExperiment is the front door: it runs before any quota is debited and before any
// footprint is computed, so it is the only place a malformed submission can still be turned away
// cheaply. Anything it lets through is acted on as fact by admission, placement and billing alike.

func maxRetries(n int) *int { return &n }

// The accelerator catalog is process-wide startup config. Registering h100 here is what makes the
// fixture below a submission the platform would actually accept — an unpriced type is rejected as
// an operator gap, which TestAnAcceleratorJobMustNameATypeThatIsPriced relies on for its other
// half.
func init() { domain.SetAcceleratorRates(map[string]float64{h100: 1.0}) }

// validSubmission is a submission that must pass. Each test spoils exactly one thing about it, so
// a failure names the rule that changed rather than whatever happened to be checked first.
func validSubmission() *domain.Experiment {
	return &domain.Experiment{
		ID:                     "exp-1",
		AgentID:                "agent-1",
		HypothesisID:           "hyp-1",
		Theory:                 "a theory",
		Objective:              "an objective",
		CodeRef:                "https://github.com/example/repo@" + strings.Repeat("a", 40),
		AcceleratorCount:       1,
		AcceleratorType:        h100,
		EstimatedDurationHours: 1,
		Job: domain.JobSpec{
			Image:            "example/image:v1",
			AcceleratorCount: 1,
			AcceleratorType:  h100,
			CPU:              "2",
			Memory:           "8Gi",
			Storage:          "10Gi",
			MaxRetries:       maxRetries(0),
		},
	}
}

func validate(t *testing.T, exp *domain.Experiment) error {
	t.Helper()
	return ValidateExperiment(exp, domain.QuotaConfig{})
}

func mustReject(t *testing.T, exp *domain.Experiment, wants string) {
	t.Helper()
	err := validate(t, exp)
	if err == nil {
		t.Fatalf("accepted, want rejected for: %s", wants)
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != ReasonMalformed {
		t.Fatalf("err = %v, want an AdmissionError with reason %q — the caller routes on the reason, not the text", err, ReasonMalformed)
	}
	if !strings.Contains(admission.Message, wants) {
		t.Fatalf("message = %q, want it to name %q so the agent can fix the field", admission.Message, wants)
	}
}

// The fixture has to pass, or every rejection below proves nothing.
func TestAWellFormedSubmissionIsAccepted(t *testing.T) {
	if err := validate(t, validSubmission()); err != nil {
		t.Fatalf("a well-formed submission was rejected: %v", err)
	}
}

// A negative quantity parses cleanly (resource.ParseQuantity accepts "-100Gi") and every cap check
// is an upper bound, so nothing downstream stops one. It has to be refused here.
//
// What makes it worth a rule of its own rather than a curiosity: the two views of the same job
// disagree about it. Footprint carries the negative through, so reserving the job credits the
// cluster with capacity that does not exist and bills the agent for less than it used, while
// NodeShapes drops the dimension entirely, so placement never sees it.
func TestANegativeResourceRequestIsRejected(t *testing.T) {
	for field, spoil := range map[string]func(*domain.JobSpec){
		"job.cpu":     func(j *domain.JobSpec) { j.CPU = "-1" },
		"job.memory":  func(j *domain.JobSpec) { j.Memory = "-100Gi" },
		"job.storage": func(j *domain.JobSpec) { j.Storage = "-10Gi" },
	} {
		exp := validSubmission()
		spoil(&exp.Job)
		mustReject(t, exp, field)
	}
}

// Per group, not on the total. The cap checks sum a dimension across groups, so a negative in one
// group cancels a real request in another — a job asking for 100Gi in one group and -100Gi in
// another sums to zero and passes every bound there is.
func TestANegativeRequestInOneGroupIsRejectedEvenWhenTheTotalLooksFine(t *testing.T) {
	exp := validSubmission()
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = "", "", ""
	exp.Job.AcceleratorCount = 0
	exp.Job.Groups = []domain.JobGroup{
		{Name: "a", Replicas: 1, AcceleratorCount: 1, AcceleratorType: h100, CPU: "2", Memory: "100Gi", Storage: "1Gi"},
		{Name: "b", Replicas: 1, AcceleratorCount: 1, AcceleratorType: h100, CPU: "2", Memory: "-100Gi", Storage: "1Gi"},
	}
	mustReject(t, exp, "job.memory")
}

// Resource requests must be explicit at submission. The footprint that selects a cluster and
// debits a quota is computed here, from these fields — leaving one to a cluster-side default means
// admitting a job whose real size the control plane never saw.
func TestAnUnspecifiedResourceDimensionIsRejectedRatherThanDefaulted(t *testing.T) {
	for field, spoil := range map[string]func(*domain.JobSpec){
		"job.cpu":     func(j *domain.JobSpec) { j.CPU = "" },
		"job.memory":  func(j *domain.JobSpec) { j.Memory = "" },
		"job.storage": func(j *domain.JobSpec) { j.Storage = "" },
	} {
		exp := validSubmission()
		spoil(&exp.Job)
		mustReject(t, exp, field)
	}
}

// "max" is a proportional share of an accelerator. A job with no accelerator has nothing to take a
// share of, so the sentinel could never be resolved — it would queue forever, waiting on a
// resolution step that only ever runs for a positive accelerator count.
func TestMaxWithoutAnAcceleratorIsRejectedRatherThanQueuedForever(t *testing.T) {
	for _, spoil := range []func(*domain.JobSpec){
		func(j *domain.JobSpec) { j.CPU = domain.MaxResourceSentinel },
		func(j *domain.JobSpec) { j.Memory = domain.MaxResourceSentinel },
		func(j *domain.JobSpec) { j.Storage = domain.MaxResourceSentinel },
	} {
		exp := validSubmission()
		exp.AcceleratorCount, exp.Job.AcceleratorCount = 0, 0
		spoil(&exp.Job)
		mustReject(t, exp, "max")
	}
}

// Zero accelerators and no CPU requests nothing at all — a job that would occupy a slot in every
// queue while being incapable of doing work.
func TestAJobRequestingNoResourcesAtAllIsRejected(t *testing.T) {
	exp := validSubmission()
	exp.AcceleratorCount, exp.Job.AcceleratorCount = 0, 0
	exp.Job.CPU = "0"
	mustReject(t, exp, "job.cpu")
}

// The ID is used verbatim in cluster resource names, so anything a DNS subdomain rejects fails at
// the cluster rather than here — after the quota is already debited.
func TestAnIDThatCannotBecomeAClusterResourceNameIsRejected(t *testing.T) {
	for _, id := range []string{"Exp-1", "exp_1", "-exp", "exp-", "exp.1/x", ""} {
		exp := validSubmission()
		exp.ID = id
		if err := validate(t, exp); err == nil {
			t.Errorf("id %q was accepted; it cannot be used verbatim as a Kubernetes resource name", id)
		}
	}
}

// code_ref is what makes a result reproducible. A branch name or a tag moves, so a standing
// recorded against one names a commit that may no longer exist — the result becomes a number
// nobody can reproduce, which is the one thing the field exists to prevent.
func TestACodeRefThatDoesNotPinACommitIsRejected(t *testing.T) {
	for _, ref := range []string{
		"https://github.com/example/repo@main",
		"https://github.com/example/repo@v1.2.3",
		"https://github.com/example/repo@" + strings.Repeat("a", 7), // short sha
		"https://github.com/example/repo",
	} {
		exp := validSubmission()
		exp.CodeRef = ref
		mustReject(t, exp, "code_ref")
	}
}

// max_retries absent and max_retries zero are different instructions, which is why it is a pointer
// — reading a missing field as 0 would silently give a job no retries at all.
func TestMissingMaxRetriesIsRejectedRatherThanReadAsZero(t *testing.T) {
	exp := validSubmission()
	exp.Job.MaxRetries = nil
	mustReject(t, exp, "job.max_retries")

	exp = validSubmission()
	exp.Job.MaxRetries = maxRetries(0)
	if err := validate(t, exp); err != nil {
		t.Fatalf("an explicit zero max_retries was rejected: %v — absent and zero are different instructions", err)
	}
}

// An accelerator job with no type has nothing to schedule against, and an unpriced type has no
// rate to bill at — an operator gap that must surface at submission, not as an experiment that
// runs for free.
func TestAnAcceleratorJobMustNameATypeThatIsPriced(t *testing.T) {
	exp := validSubmission()
	exp.Job.AcceleratorType = ""
	mustReject(t, exp, "job.accelerator_type")

	exp = validSubmission()
	exp.Job.AcceleratorType = "example.com/product=never-priced"
	exp.AcceleratorType = exp.Job.AcceleratorType
	mustReject(t, exp, "acch_rate")
}

// A negative accelerator count would credit the cluster an accelerator on every reservation.
func TestANegativeAcceleratorCountIsRejected(t *testing.T) {
	exp := validSubmission()
	exp.AcceleratorCount = -1
	mustReject(t, exp, "accelerator_count")
}

// Duration is the denominator of every per-hour rate this job is billed at. Zero or negative makes
// that rate undefined or inverted.
func TestANonPositiveEstimatedDurationIsRejected(t *testing.T) {
	for _, hours := range []float64{0, -1} {
		exp := validSubmission()
		exp.EstimatedDurationHours = hours
		mustReject(t, exp, "estimated_duration_hours")
	}
}

// Every identifying field is required: a submission missing one cannot be attributed, ranked, or
// explained afterwards.
func TestEveryRequiredIdentityFieldIsEnforced(t *testing.T) {
	for field, spoil := range map[string]func(*domain.Experiment){
		"agent_id":      func(e *domain.Experiment) { e.AgentID = "" },
		"hypothesis_id": func(e *domain.Experiment) { e.HypothesisID = "" },
		"theory":        func(e *domain.Experiment) { e.Theory = "" },
		"objective":     func(e *domain.Experiment) { e.Objective = "" },
		"job.image":     func(e *domain.Experiment) { e.Job.Image = "" },
	} {
		exp := validSubmission()
		spoil(exp)
		mustReject(t, exp, field)
	}
}

// The per-job caps exist so one absurd submission cannot consume an entire budget before anything
// downstream gets a say. They are applied to the whole job — replicas included — because that is
// what the cluster is asked to find room for.
func TestPerJobCapsCountEveryReplicaNotOneOfThem(t *testing.T) {
	exp := validSubmission()
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = "", "", ""
	exp.Job.AcceleratorCount = 0
	exp.Job.Groups = []domain.JobGroup{
		{Name: "w", Replicas: 4, AcceleratorCount: 1, AcceleratorType: h100, CPU: "8", Memory: "8Gi", Storage: "1Gi"},
	}
	caps := domain.QuotaConfig{MaxCPUCoresPerJob: 20} // 4 x 8 = 32 cores; one replica alone would pass

	err := ValidateExperiment(exp, caps)
	if err == nil {
		t.Fatal("a job whose replicas together exceed the CPU cap was accepted — the cap must bound what the cluster is actually asked to run")
	}
	if !strings.Contains(err.Error(), "job.cpu") {
		t.Fatalf("err = %v, want it to name job.cpu", err)
	}
}
