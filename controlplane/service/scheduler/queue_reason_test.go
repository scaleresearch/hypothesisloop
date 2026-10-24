package scheduler

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func reasonCode(stored string) string {
	code, _ := domain.SplitQueueReason(stored)
	return code
}

// notAdmittedCodes reads every NotAdmitted* constant out of the domain package's source, so a code
// added there is one this test knows of without being told.
func notAdmittedCodes(t *testing.T) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "../../domain/constants.go", nil, 0)
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
// and the detail of job.held say, and a job told its code alone learns nothing of what it waits
// for. Each code is produced here the way the scheduler produces it.
func TestEveryNotAdmittedReasonIsStoredWithASentence(t *testing.T) {
	exp := holdJob("job", domain.CapacityGuaranteed, h100, 1, "1")
	exp.RanOnAcceleratorType = h100
	imageChoice := map[string]domain.Cluster{"rented": {Name: "rented"}, "metal": {Name: "metal"}}
	amd64Job := *exp
	amd64Job.ImageArchitecture = "amd64"
	short := domain.NewFootprint()
	short.Add(domain.ResourceKey{Kind: domain.ResourceKindMemory}, 1<<30)
	gang := *h100Job("gang", "2")
	gang.Job.NumNodes, gang.AcceleratorCount = 2, 2
	networkSizes := map[string]map[string]int64{"h100-0": size(8000, 32, 100), "h100-1": size(8000, 32, 100)}
	noNetwork := map[string]map[string]string{}
	for node := range networkSizes {
		noNetwork[node] = map[string]string{domain.NodeSlotLabel: node, domain.NodeSlotAcceleratorsLabel: "1", h100LabelKey: "NVIDIA-H100-80GB-HBM3",
			domain.NodeSlotNetworkLabel: "0", domain.NodeSlotNoNetworkLabel: "no vast cluster has 2 machines of the shape"}
	}
	produced := map[string][]string{
		domain.NotAdmittedAwaitingAdmission: {domain.QueueReasonAwaitingAdmission},
		domain.NotAdmittedCapacityUnavailable: {
			capacityTakenReason(),
			notAdmittedReasonForSubmitError(errAdmissionCapacityChanged),
			notAdmittedReasonFor(false, false, false, short, "cluster-a", holders{}),
			notAdmittedReasonFor(false, true, false, short, "cluster-a", holders{}),
			notAdmittedReasonFor(false, false, false, domain.NewFootprint(), "", holders{}),
			notAdmittedReasonFor(false, false, true, short, "cluster-a", holders{}),
			waitingForSlotsReason("cluster-a", []string{"h100-0"}),
			waitingForSlotsReason("cluster-a", []string{"h100-0", "h100-1"}),
			noNetworkReason(&gang, "cluster-a", networkSizes, networkSizes, noNetwork),
		},
		domain.NotAdmittedOutranked:               {notAdmittedReasonFor(true, false, false, short, "cluster-a", holders{})},
		domain.NotAdmittedStageCut:                {stageCutReason()},
		domain.NotAdmittedJobTooLong:              {jobTooLongReason(2, 0.5)},
		domain.NotAdmittedWorkloadCreation:        {workloadCreationReason(), notAdmittedReasonForSubmitError(errors.New("boom"))},
		domain.NotAdmittedInsufficientQuota:       {ranOnTypeUncoveredReason(exp)},
		domain.NotAdmittedNoMultiNodeCluster:      {noMultiNodeClusterReason(2)},
		domain.NotAdmittedNoClusterForOrg:         {noClusterForOrgReason()},
		domain.NotAdmittedClustersDisabled:        {clustersDisabledReason()},
		domain.NotAdmittedWaitingForScaleUp:       {waitingForScaleUpReason("cluster-a")},
		domain.NotAdmittedNoScalableCapacity:      {noScalableCapacityReason()},
		domain.NotAdmittedConcurrencyCap:          {concurrencyCapReason(), notAdmittedReasonForSubmitError(capExceeded{errors.New("cap")})},
		domain.NotAdmittedExceedsAcceleratorShare: {shareMisfit("cpu", "9", "2", 1, "accelerator", "cluster-a")},
		domain.NotAdmittedExceedsClusterCapacity:  {installed{registered: imageChoice, totals: map[string]domain.Footprint{"metal": {}}}.neverFitsReason(exp)},
		domain.NotAdmittedNoClusterForArchitecture: {installed{registered: imageChoice, totals: map[string]domain.Footprint{"metal": {}},
			nodeLabels: map[string]map[string]map[string]string{"metal": {"box": {domain.NodeArchitectureLabel: "arm64"}}}}.neverFitsReason(&amd64Job)},
		domain.NotAdmittedOrgComputeLimit:     {notAdmittedReasonForSubmitError(computeLimitReached{&domain.ClaimRefusal{Code: domain.NotAdmittedOrgComputeLimit, Holder: "acme", Held: 7, Limit: 8, Needs: 2}})},
		domain.NotAdmittedProjectComputeLimit: {notAdmittedReasonForSubmitError(computeLimitReached{&domain.ClaimRefusal{Code: domain.NotAdmittedProjectComputeLimit, Holder: "vision", Held: 3, Limit: 4, Needs: 2}})},
		domain.NotAdmittedExceedsProjectComputeLimit: {installed{registered: map[string]domain.Cluster{"platform": {Name: "platform", OrgID: domain.PlatformOrgID}},
			totals: map[string]domain.Footprint{"platform": {}}, limits: map[string]domain.ComputeLimit{"": {Org: 1, Project: 0.5}}}.neverFitsReason(exp)},
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

// A gang spread over more hosts than a cluster has never runs there, and its reason says how many
// hosts it needs, how many the cluster has, and how to let its nodes share one.
func TestNeverFitsReasonOfASpreadGangOverTooFewHosts(t *testing.T) {
	exp := holdJob("gang", domain.CapacityBurst, h100, 1, "1")
	exp.Job.NumNodes = 3
	exp.AcceleratorCount = 3
	host := map[string]int64{domain.NodeResourceCPUMillicores: 8000, domain.NodeResourceMemoryBytes: 8 << 30, domain.NodeResourceStorageBytes: 8 << 30}
	in := installed{
		registered: map[string]domain.Cluster{"k8s": {Name: "k8s"}},
		totals:     map[string]domain.Footprint{"k8s": domain.CapacityFootprint(16, map[string]int64{string(h100): 8}, 16<<30, 16<<30)},
		nodeTotals: map[string]map[string]map[string]int64{"k8s": {"a": host, "b": host}},
		multiNode:  map[string]bool{"k8s": true},
	}
	reason := in.neverFitsReason(exp)
	if code := reasonCode(reason); code != domain.NotAdmittedExceedsClusterCapacity {
		t.Fatalf("reason = %q, want %s", reason, domain.NotAdmittedExceedsClusterCapacity)
	}
	for _, want := range []string{"3 hosts", "2 of its 2 hosts", "topology.spread_across_hosts: false"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not say %q", reason, want)
		}
	}

	stack := false
	exp.Job.Topology = &domain.TopologySpec{SpreadAcrossHosts: &stack}
	if reason := in.neverFitsReason(exp); reason != "" {
		t.Fatalf("a gang whose nodes may share a host is refused: %q", reason)
	}
}
