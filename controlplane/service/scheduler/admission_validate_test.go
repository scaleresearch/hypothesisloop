package scheduler

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
		CodeRef:                "https://github.com/example/repo@" + strings.Repeat("a", 40),
		AcceleratorCount:       1,
		AcceleratorType:        h100,
		EstimatedDurationHours: 1,
		Job: domain.JobSpec{
			Command:          []string{"python", "train.py"},
			AcceleratorCount: 1,
			AcceleratorType:  h100,
			CPU:              "2",
			Memory:           "8Gi",
			Storage:          "10Gi",
			MaxRetries:       maxRetries(0),
		},
	}
}

// testCaps states every limit, as the settings must, each above what a test's job asks for.
var testCaps = domain.QuotaConfig{MaxSubmissionsPerHour: 1000, MaxAcceleratorCountPerJob: 64, MaxCPUCoresPerJob: 1024, MaxRAMGBPerJob: 4096, MaxStorageGBPerJob: 8192}

func validate(t *testing.T, exp *domain.Experiment) error {
	t.Helper()
	return ValidateExperiment(exp, testCaps)
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

func TestAnUnusableShmSizeIsRejected(t *testing.T) {
	for _, size := range []string{"4GB", "-1Gi", "0"} {
		exp := validSubmission()
		exp.Job.ShmSize = size
		mustReject(t, exp, "job.shm_size")
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
	mustReject(t, exp, `job.groups["b"].memory`)
}

// A job leaves out its retries and gets none. What submission wrote is what every later reader
// of the job sees.
func TestWhatAJobLeavesOutIsZeroRetries(t *testing.T) {
	exp := validSubmission()
	exp.Job.MaxRetries = nil
	if err := validate(t, exp); err != nil || exp.Job.MaxRetries == nil || *exp.Job.MaxRetries != 0 {
		t.Fatalf("max_retries = %v, err %v; want 0", exp.Job.MaxRetries, err)
	}
	if exp.Job.CPU != "2" || exp.Job.Memory != "8Gi" || exp.Job.Storage != "10Gi" {
		t.Errorf("cpu, memory, storage = %q, %q, %q; want them as stated", exp.Job.CPU, exp.Job.Memory, exp.Job.Storage)
	}

	exp = validSubmission()
	exp.Job.MaxRetries = maxRetries(-1)
	mustReject(t, exp, "job.max_retries")
}

// A job holds what it states. A chip job that left memory out was given the whole memory of its
// node, and a job that used 2 cores of 32 kept every other job off the machine. Its disk a node
// offers only as far as it is free, which no share of its size says. So each node's cpu, memory
// and storage are stated as quantities, by every job and every group.
func TestAJobStatesItsCPUMemoryAndStorageAsQuantities(t *testing.T) {
	grouped := func(group domain.JobGroup) func(*domain.Experiment) {
		return func(e *domain.Experiment) {
			e.Job.CPU, e.Job.Memory, e.Job.Storage, e.Job.AcceleratorCount = "", "", "", 0
			group.Name, group.Replicas, group.AcceleratorCount, group.AcceleratorType = "learner", 1, 1, h100
			e.Job.Groups = []domain.JobGroup{group}
		}
	}
	for name, tc := range map[string]struct {
		spoil  func(*domain.Experiment)
		reason string
		field  string
	}{
		"cpu left out":     {func(e *domain.Experiment) { e.Job.CPU = "" }, ReasonRequired, "job.cpu is required"},
		"cpu a share":      {func(e *domain.Experiment) { e.Job.CPU = "max" }, ReasonRequired, `job.cpu is "max"`},
		"memory left out":  {func(e *domain.Experiment) { e.Job.Memory = "" }, ReasonRequired, "job.memory is required"},
		"memory a share":   {func(e *domain.Experiment) { e.Job.Memory = "max" }, ReasonRequired, `job.memory is "max"`},
		"storage left out": {func(e *domain.Experiment) { e.Job.Storage = "" }, ReasonRequired, "job.storage is required"},
		"storage a share":  {func(e *domain.Experiment) { e.Job.Storage = "max" }, ReasonRequired, `job.storage is "max"`},
		"nothing stated on CPU": {func(e *domain.Experiment) {
			e.AcceleratorType, e.Job.AcceleratorType, e.AcceleratorCount, e.Job.AcceleratorCount = "", domain.CPUType, 0, 4
			e.Job.CPU = ""
		}, ReasonRequired, "job.cpu is required"},
		"cpu left out of a group":     {grouped(domain.JobGroup{Memory: "1Gi", Storage: "1Gi"}), ReasonRequired, `job.groups["learner"].cpu is required`},
		"memory a share in a group":   {grouped(domain.JobGroup{CPU: "1", Memory: "max", Storage: "1Gi"}), ReasonRequired, `job.groups["learner"].memory is "max"`},
		"storage left out of a group": {grouped(domain.JobGroup{CPU: "1", Memory: "1Gi"}), ReasonRequired, `job.groups["learner"].storage is required`},
	} {
		exp := validSubmission()
		tc.spoil(exp)
		var refused *AdmissionError
		if err := ValidateExperiment(exp, testCaps); !errors.As(err, &refused) || refused.Reason != tc.reason || !strings.Contains(refused.Message, tc.field) {
			t.Errorf("%s: err = %v, want %s naming the field (%s)", name, err, tc.reason, tc.field)
		}
	}
	for _, reason := range []string{ReasonRequired, ReasonRequired, ReasonRequired} {
		if got := AdmissionStatus(reason); got != http.StatusUnprocessableEntity {
			t.Errorf("%s answers %d, want 422", reason, got)
		}
	}
}

// Budget, quota, fairness and the concurrency cap count the units of a priced type a job holds. A
// job that held none ran outside all four, so there is none: work that needs no accelerator
// states its cores and holds them as units of the CPU type (see cpu_type_test.go).
func TestAJobThatHoldsNoUnitOfAPricedTypeIsRejected(t *testing.T) {
	const wants = "asks for nothing the catalog prices"
	for name, spoil := range map[string]func(*domain.Experiment){
		"a type and no count": func(*domain.Experiment) {},
		"no cores either":     func(e *domain.Experiment) { e.Job.CPU = "0" },
		"groups that all hold none": func(e *domain.Experiment) {
			e.Job.CPU, e.Job.Memory, e.Job.Storage = "", "", ""
			e.Job.Groups = []domain.JobGroup{
				{Name: "a", Replicas: 2, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
				{Name: "b", Replicas: 1, CPU: "1", Memory: "1Gi", Storage: "1Gi"},
			}
		},
	} {
		exp := validSubmission()
		exp.AcceleratorCount, exp.Job.AcceleratorCount = 0, 0
		spoil(exp)
		t.Run(name, func(t *testing.T) { mustReject(t, exp, wants) })
	}
}

// A group that holds no device rides beside one that does, as a job's cores ride beside its
// accelerators: the job holds priced units and is counted by them.
func TestAGroupWithNoAcceleratorIsAcceptedBesideOneThatHoldsSome(t *testing.T) {
	exp := validSubmission()
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage, exp.Job.AcceleratorCount = "", "", "", 0
	exp.Job.Groups = []domain.JobGroup{
		{Name: "learner", Replicas: 1, AcceleratorCount: 1, CPU: "8", Memory: "8Gi", Storage: "1Gi"},
		{Name: "actors", Replicas: 4, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
	}
	if err := validate(t, exp); err != nil {
		t.Fatalf("a learner with one accelerator and four actors with none: %v", err)
	}
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

// Duration is the denominator of every per-hour rate this job is billed at, and a negative one
// inverts that rate. One left out is the submit's to give or to refuse (required).
func TestANegativeEstimatedDurationIsRejected(t *testing.T) {
	exp := validSubmission()
	exp.EstimatedDurationHours = -1
	mustReject(t, exp, "estimated_duration_hours")
}

// Every identifying field is required: a submission missing one cannot be attributed, ranked, or
// explained afterwards.
func TestEveryRequiredIdentityFieldIsEnforced(t *testing.T) {
	for field, spoil := range map[string]func(*domain.Experiment){
		"agent_id":      func(e *domain.Experiment) { e.AgentID = "" },
		"hypothesis_id": func(e *domain.Experiment) { e.HypothesisID = "" },
		"theory":        func(e *domain.Experiment) { e.Theory = "" },
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
	caps := testCaps
	caps.MaxCPUCoresPerJob = 20 // 4 x 8 = 32 cores; one replica alone would pass

	err := ValidateExperiment(exp, caps)
	if err == nil {
		t.Fatal("a job whose replicas together exceed the CPU cap was accepted — the cap must bound what the cluster is actually asked to run")
	}
	if !strings.Contains(err.Error(), "job.cpu") {
		t.Fatalf("err = %v, want it to name job.cpu", err)
	}
}

// capacity_tier has a same-named-but-different sibling enum (domain.QuotaTier, "guaranteed" or
// "burst_only", used at signup) that every experiment's job.yaml template used to mislabel this
// field with — a submission carrying that value must be rejected here, not reach Postgres and
// surface as a raw enum SQLSTATE.
func TestCapacityTierAcceptsOnlyItsOwnEnum(t *testing.T) {
	for _, tier := range []domain.CapacityTier{"", domain.CapacityGuaranteed, domain.CapacityBurst} {
		exp := validSubmission()
		exp.CapacityTier = tier
		if err := validate(t, exp); err != nil {
			t.Fatalf("capacity_tier = %q was rejected: %v", tier, err)
		}
		// Left out it is guaranteed, written on the job the submit answers with.
		if tier == "" && exp.CapacityTier != domain.CapacityGuaranteed {
			t.Fatalf("capacity_tier left out is answered as %q, want %q", exp.CapacityTier, domain.CapacityGuaranteed)
		}
	}
	for _, tier := range []domain.CapacityTier{"burst_only", "guaranteed ", "Burst"} {
		exp := validSubmission()
		exp.CapacityTier = tier
		mustReject(t, exp, "capacity_tier")
	}
}

// The slot a job runs in is admission's to choose. A submitted selector naming it, or any other
// label of the platform's, would let a submitter place the job, and on a cluster that has no
// such node the row dropped out of the plan the claim proves.
func TestASubmittedNodeSelectorMayNotNameAPlatformLabel(t *testing.T) {
	for _, label := range []string{domain.NodeSlotLabel, domain.NodeSlotAcceleratorsLabel, domain.NodeLabelReservedPrefix + "anything"} {
		exp := validSubmission()
		exp.Job.NodeSelector = map[string]string{"zone": "a", label: "h100-0"}
		mustReject(t, exp, label)
	}
	exp := validSubmission()
	exp.Job.NodeSelector = map[string]string{"zone": "a", "kubernetes.io/hostname": "node-1"}
	if err := validate(t, exp); err != nil {
		t.Fatalf("an ordinary node selector was rejected: %v", err)
	}
}

func TestASecretEnvNameAlsoSetInTheOpenIsRejected(t *testing.T) {
	exp := validSubmission()
	exp.Job.Env = map[string]string{"HF_TOKEN": "open"}
	exp.SecretEnv = map[string]string{"HF_TOKEN": "secret"}
	mustReject(t, exp, `"HF_TOKEN" is in both job.env and job.secret_env`)

	exp = validSubmission()
	exp.SecretEnv = map[string]string{"HF-TOKEN": "secret"}
	mustReject(t, exp, "not an environment variable name")

	exp = validSubmission()
	exp.SecretEnv = map[string]string{"HF_TOKEN": "secret"}
	if err := validate(t, exp); err != nil {
		t.Fatalf("a job with a secret_env of its own names: got = %v, want accepted", err)
	}
}

// Every node runs the job's command in its code, so each states one: the job's, or its group's.
func TestAJobWithNoCommandIsRefused(t *testing.T) {
	exp := validSubmission()
	exp.Job.Command = nil
	var refused *AdmissionError
	if !errors.As(validate(t, exp), &refused) || refused.Reason != ReasonRequired || refused.Field != "job.command" {
		t.Fatalf("a job with no command: %+v", refused)
	}
	grouped := validSubmission()
	grouped.Job.Command, grouped.Job.CPU, grouped.Job.Memory, grouped.Job.Storage, grouped.Job.AcceleratorCount = nil, "", "", "", 0
	grouped.Job.Groups = []domain.JobGroup{{Name: "learner", Replicas: 1, CPU: "2", Memory: "8Gi", Storage: "10Gi", AcceleratorCount: 1, Command: []string{"python", "learn.py"}}}
	if err := validate(t, grouped); err != nil {
		t.Fatalf("a grouped job whose groups each state a command: %v", err)
	}
}

// The launch script fetches the job's code with the token and repository the platform sets, and
// keeps the command for the stop grace it sets, so a job may set none of the names the platform
// does, in the open or as a secret.
func TestAJobThatSetsAVariableThePlatformSetsIsRefused(t *testing.T) {
	for _, name := range []string{"HYPOTHESISLOOP_TOKEN", "HYPOTHESISLOOP_GIT_URL", "HYPOTHESISLOOP_CODE_REF", "HYPOTHESISLOOP_STOP_GRACE", "AWS_SECRET_ACCESS_KEY", "MASTER_ADDR"} {
		open := validSubmission()
		open.Job.Env = map[string]string{name: "x"}
		mustReject(t, open, "job.env may not set "+name)

		secret := validSubmission()
		secret.SecretEnv = map[string]string{name: "x"}
		mustReject(t, secret, "job.secret_env may not set "+name)
	}
	grouped := validSubmission()
	grouped.Job.CPU, grouped.Job.Memory, grouped.Job.Storage, grouped.Job.AcceleratorCount = "", "", "", 0
	grouped.Job.Groups = []domain.JobGroup{{Name: "learner", Replicas: 1, CPU: "2", Memory: "8Gi", Storage: "10Gi", AcceleratorCount: 1, Env: map[string]string{"HYPOTHESISLOOP_TOKEN": "x"}}}
	mustReject(t, grouped, `job.groups["learner"].env may not set HYPOTHESISLOOP_TOKEN`)

	fine := validSubmission()
	fine.Job.Env = map[string]string{"LR": "0.1"}
	fine.SecretEnv = map[string]string{"HF_TOKEN": "x"}
	if err := validate(t, fine); err != nil {
		t.Fatalf("a job with its own variables: %v", err)
	}
}
