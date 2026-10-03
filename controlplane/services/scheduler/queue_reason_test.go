package scheduler

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func reasonCode(stored string) string {
	code, _ := domain.SplitQueueReason(stored)
	return code
}

// notAdmittedCodes reads every NotAdmitted* constant out of the domain package's source, so a code
// added there is one this test knows of without being told.
func notAdmittedCodes(t *testing.T) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../../shared/domain/constants.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "NotAdmitted") {
				continue
			}
			value, err := strconv.Unquote(spec.Values[i].(*ast.BasicLit).Value)
			if err != nil {
				t.Fatalf("%s: %v", name.Name, err)
			}
			codes[value] = name.Name
		}
		return true
	})
	if len(codes) < 15 {
		t.Fatalf("read %d not-admitted codes from the domain package, want all of them: %v", len(codes), codes)
	}
	return codes
}

type capExceeded struct{ error }

func (capExceeded) ConcurrencyCapExceeded() bool { return true }

// Every reason a queued job can carry is stored with a sentence: it is what not_admitted_message
// and the detail of job.held say, and a job held 5 minutes behind an unfiled summary was told
// "summary_gate" and nothing else. Each code is produced here the way the scheduler produces it.
func TestEveryNotAdmittedReasonIsStoredWithASentence(t *testing.T) {
	exp := holdJob("job", domain.CapacityGuaranteed, h100, 1, "1")
	exp.RanOnAcceleratorType = h100
	unnamed := holdJob("job", domain.CapacityGuaranteed, h100, 1, "1")
	unnamed.Job.Image = ""
	imageChoice := map[string]domain.Cluster{"rented": {Name: "rented", JobImage: "registry.test/only:1"}, "metal": {Name: "metal"}}
	short := domain.NewFootprint()
	short.Add(domain.ResourceKey{Kind: domain.ResourceKindMemory}, 1<<30)
	produced := map[string][]string{
		domain.NotAdmittedAwaitingAdmission: {domain.QueueReasonAwaitingAdmission},
		domain.NotAdmittedCapacityUnavailable: {
			capacityTakenReason(),
			notAdmittedReasonForSubmitError(errAdmissionCapacityChanged),
			notAdmittedReasonFor(false, false, false, short, "cluster-a", holders{}),
			notAdmittedReasonFor(false, true, false, short, "cluster-a", holders{}),
			notAdmittedReasonFor(false, false, false, domain.NewFootprint(), "", holders{}),
			notAdmittedReasonFor(false, false, true, short, "cluster-a", holders{}),
			waitingForSlotReason("cluster-a", "h100-0"),
		},
		domain.NotAdmittedOutranked:               {notAdmittedReasonFor(true, false, false, short, "cluster-a", holders{})},
		domain.NotAdmittedSummaryGate:             {summaryGateReason([]string{"job-a"}), summaryGateReason([]string{"job-a", "job-b", "job-c", "job-d"})},
		domain.NotAdmittedStageCut:                {stageCutReason()},
		domain.NotAdmittedJobTooLong:              {jobTooLongReason(2, 0.5)},
		domain.NotAdmittedWorkloadCreation:        {workloadCreationReason(), notAdmittedReasonForSubmitError(errors.New("boom"))},
		domain.NotAdmittedInsufficientQuota:       {ranOnTypeUncoveredReason(exp)},
		domain.NotAdmittedNoMultiNodeCluster:      {noMultiNodeClusterReason(2)},
		domain.NotAdmittedNoClusterForOrg:         {noClusterForOrgReason()},
		domain.NotAdmittedClustersDisabled:        {clustersDisabledReason()},
		domain.NotAdmittedNoClusterForImage:       {
			noClusterRunsImageReason([]domain.Cluster{{Name: "rented", JobImage: "registry.test/only:1"}}, exp),
			installed{registered: imageChoice, totals: map[string]domain.Footprint{"rented": {}, "metal": domain.CapacityFootprint(8, map[string]int64{h100: 1}, 64<<30, 64<<30)}}.neverFitsReason(unnamed),
		},
		domain.NotAdmittedWaitingForScaleUp:       {waitingForScaleUpReason("cluster-a")},
		domain.NotAdmittedNoScalableCapacity:      {noScalableCapacityReason()},
		domain.NotAdmittedConcurrencyCap:          {concurrencyCapReason(), notAdmittedReasonForSubmitError(capExceeded{errors.New("cap")})},
		domain.NotAdmittedExceedsAcceleratorShare: {shareMisfit("cpu", "9", "2", 1, "cluster-a")},
		domain.NotAdmittedExceedsClusterCapacity:  {installed{registered: imageChoice, totals: map[string]domain.Footprint{"rented": {}}}.neverFitsReason(unnamed)},
	}
	for code, name := range notAdmittedCodes(t) {
		if len(produced[code]) == 0 {
			t.Errorf("%s (%s) has no sentence: word it where the scheduler writes it and produce it here", name, code)
		}
		for _, stored := range produced[code] {
			if got, sentence := domain.SplitQueueReason(stored); got != code || len(sentence) < 20 {
				t.Errorf("%s is stored as %q, want %q and a sentence", name, stored, code+": ...")
			}
		}
	}
	for code := range produced {
		if _, known := notAdmittedCodes(t)[code]; !known {
			t.Errorf("%q is produced and is no code of the domain package", code)
		}
	}
}

func TestTheSummaryGateNamesTheJobsOwedAndTheCommandThatFilesOne(t *testing.T) {
	for want, owed := range map[string][]string{
		"summary_gate: job job-a of this agent is COMPLETED and has no summary: hl job summary job-a --text T files one, and this job is admitted once none is owed":                             {"job-a"},
		"summary_gate: jobs job-a, job-b of this agent are COMPLETED and have no summary: hl job summary job-a --text T files one, and this job is admitted once none is owed":                   {"job-a", "job-b"},
		"summary_gate: jobs job-a, job-b, job-c and 2 more of this agent are COMPLETED and have no summary: hl job summary job-a --text T files one, and this job is admitted once none is owed": {"job-a", "job-b", "job-c", "job-d", "job-e"},
	} {
		if got := summaryGateReason(owed); got != want {
			t.Errorf("summaryGateReason(%v) =\n%q, want\n%q", owed, got, want)
		}
	}
}
