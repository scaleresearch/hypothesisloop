package scheduler

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A CPU class is a priced type whose unit is one core. These tests hold what that one fact
// changes at submission and in the spec a runtime is given, and that a submission for an
// accelerator type reads as it did before the catalog knew of kinds.

const (
	cpuClass      = "cpu.hypothesisloop.dev/class=EPYC-9354"
	otherCPUClass = "cpu.hypothesisloop.dev/class=Xeon-8480"
)

// withCPUClasses prices two CPU classes beside h100 and l40 for one test. The catalog is
// process-wide, so it is put back as the package's init left it.
func withCPUClasses(t *testing.T) {
	t.Helper()
	domain.SetAcceleratorRates(map[string]float64{h100: 1.0, l40: 0.25, cpuClass: 0.01, otherCPUClass: 0.02})
	domain.SetAcceleratorKinds(map[string]domain.TypeKind{cpuClass: domain.TypeKindCPU, otherCPUClass: domain.TypeKindCPU})
	t.Cleanup(func() {
		domain.SetAcceleratorRates(map[string]float64{h100: 1.0})
		domain.SetAcceleratorKinds(nil)
	})
}

// cpuSubmission asks for cores of the class and says nothing else about its size.
func cpuSubmission(cores int) *domain.Experiment {
	exp := validSubmission()
	exp.AcceleratorType, exp.AcceleratorCount = cpuClass, cores
	exp.Job.AcceleratorType, exp.Job.AcceleratorCount = cpuClass, cores
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = "", "", ""
	return exp
}

func TestTheCountOfACPUTypeIsTheJobsCores(t *testing.T) {
	withCPUClasses(t)
	exp := cpuSubmission(8)
	if err := validate(t, exp); err != nil {
		t.Fatalf("a job that names only its cores: %v", err)
	}
	if exp.Job.CPU != "8" || exp.Job.Memory != domain.MaxResourceSentinel || exp.Job.Storage != domain.MaxResourceSentinel {
		t.Errorf("cpu, memory, storage = %q, %q, %q, want 8 cores and the node's share of the rest", exp.Job.CPU, exp.Job.Memory, exp.Job.Storage)
	}
	if exp.AcceleratorType != cpuClass || exp.AcceleratorCount != 8 {
		t.Errorf("admitted as %d of %q, want 8 of the class", exp.AcceleratorCount, exp.AcceleratorType)
	}
}

func TestACPUJobMayStateItsCoresAgainAndItsOwnMemory(t *testing.T) {
	withCPUClasses(t)
	for _, cpu := range []string{"8", "8000m", "8.0"} {
		exp := cpuSubmission(8)
		exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = cpu, "4Gi", "20Gi"
		if err := validate(t, exp); err != nil {
			t.Fatalf("cpu %q beside a count of 8: %v", cpu, err)
		}
		if exp.Job.CPU != cpu || exp.Job.Memory != "4Gi" || exp.Job.Storage != "20Gi" {
			t.Errorf("cpu %q: the job was rewritten to %q, %q, %q", cpu, exp.Job.CPU, exp.Job.Memory, exp.Job.Storage)
		}
	}
}

// Cores beyond the count would be cores nothing is charged for.
func TestACPUJobThatAsksForOtherCoresThanItsCountIsRejected(t *testing.T) {
	withCPUClasses(t)
	for _, cpu := range []string{"16", "4", "7500m", "0", domain.MaxResourceSentinel} {
		exp := cpuSubmission(8)
		exp.Job.CPU = cpu
		mustReject(t, exp, "the count is the node's cores")
	}
	exp := cpuSubmission(8)
	exp.Job.CPU = "eight"
	mustReject(t, exp, "job.cpu")
}

func TestACPUJobNeedsAPositiveCount(t *testing.T) {
	withCPUClasses(t)
	exp := cpuSubmission(0)
	exp.Job.CPU = "4"
	mustReject(t, exp, "job.accelerator_count must be at least 1")
}

func TestEachGroupOfACPUJobHasItsCountAsItsCores(t *testing.T) {
	withCPUClasses(t)
	grouped := func(groups ...domain.JobGroup) *domain.Experiment {
		exp := cpuSubmission(0)
		exp.Job.AcceleratorType = ""
		exp.Job.Groups = groups
		exp.AcceleratorCount = exp.Job.TotalAccelerators()
		return exp
	}

	exp := grouped(
		domain.JobGroup{Name: "driver", Replicas: 1, AcceleratorCount: 2, AcceleratorType: cpuClass},
		domain.JobGroup{Name: "workers", Replicas: 3, AcceleratorCount: 4, CPU: "4", Memory: "2Gi", Storage: "1Gi"},
	)
	if err := validate(t, exp); err != nil {
		t.Fatalf("a grouped job on a CPU class: %v", err)
	}
	want := []domain.JobGroup{
		{Name: "driver", Replicas: 1, AcceleratorCount: 2, AcceleratorType: cpuClass, CPU: "2", Memory: domain.MaxResourceSentinel, Storage: domain.MaxResourceSentinel},
		{Name: "workers", Replicas: 3, AcceleratorCount: 4, CPU: "4", Memory: "2Gi", Storage: "1Gi"},
	}
	if !reflect.DeepEqual(exp.Job.Groups, want) {
		t.Errorf("groups = %+v, want %+v", exp.Job.Groups, want)
	}
	if exp.AcceleratorCount != 14 || exp.AcceleratorType != cpuClass {
		t.Errorf("admitted as %d of %q, want the 14 cores of all replicas", exp.AcceleratorCount, exp.AcceleratorType)
	}

	// A group with cores and no count would run them unpriced beside the groups that pay.
	mustReject(t, grouped(
		domain.JobGroup{Name: "driver", Replicas: 1, AcceleratorCount: 2, AcceleratorType: cpuClass},
		domain.JobGroup{Name: "free", Replicas: 8, CPU: "4", Memory: "2Gi", Storage: "1Gi"},
	), `job.groups["free"].accelerator_count must be at least 1`)
	mustReject(t, grouped(
		domain.JobGroup{Name: "driver", Replicas: 1, AcceleratorCount: 2, AcceleratorType: cpuClass, CPU: "6"},
	), `job.groups["driver"].cpu`)
}

// One count cannot be cores on one type and devices on another, and on the device the cores the
// count had set would be charged to nobody.
func TestAnAlternativeTypeIsOfTheKindOfTheTypeAskedFor(t *testing.T) {
	withCPUClasses(t)

	exp := cpuSubmission(4)
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{otherCPUClass, cpuClass}
	if err := validate(t, exp); err != nil {
		t.Fatalf("a CPU job that accepts another CPU class: %v", err)
	}

	exp = cpuSubmission(4)
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{otherCPUClass, h100}
	mustReject(t, exp, "of kind accelerator")

	exp = validSubmission()
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{l40, cpuClass}
	mustReject(t, exp, "of kind cpu")
}

// The same submissions, with the catalog holding CPU classes and without: nothing about an
// accelerator job is read differently.
func TestAnAcceleratorSubmissionIsValidatedAsBeforeBesideACPUClass(t *testing.T) {
	submissions := map[string]func() *domain.Experiment{
		"plain": validSubmission,
		"max shares": func() *domain.Experiment {
			exp := validSubmission()
			exp.Job.CPU, exp.Job.Storage = domain.MaxResourceSentinel, domain.MaxResourceSentinel
			return exp
		},
		"more cores than devices": func() *domain.Experiment {
			exp := validSubmission()
			exp.Job.CPU = "32"
			return exp
		},
		"no cpu": func() *domain.Experiment {
			exp := validSubmission()
			exp.Job.CPU = ""
			return exp
		},
		"grouped with a group that holds no device": func() *domain.Experiment {
			exp := validSubmission()
			exp.Job.CPU, exp.Job.Memory, exp.Job.Storage, exp.Job.AcceleratorCount = "", "", "", 0
			exp.Job.Groups = []domain.JobGroup{
				{Name: "learner", Replicas: 1, AcceleratorCount: 1, CPU: "8", Memory: "8Gi", Storage: "1Gi"},
				{Name: "actors", Replicas: 4, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
			}
			return exp
		},
	}
	type outcome struct {
		err string
		exp domain.Experiment
	}
	read := func() map[string]outcome {
		out := map[string]outcome{}
		for name, build := range submissions {
			exp := build()
			var text string
			if err := validate(t, exp); err != nil {
				text = err.Error()
			}
			out[name] = outcome{text, *exp}
		}
		return out
	}
	before := read()
	withCPUClasses(t)
	if after := read(); !reflect.DeepEqual(before, after) {
		t.Errorf("accelerator submissions read differently once the catalog holds a CPU class:\nbefore %+v\nafter  %+v", before, after)
	}
	if before["no cpu"].err == "" || before["plain"].err != "" {
		t.Errorf("the comparison proves nothing: %+v", before)
	}
}

// A node that publishes as many units of the class as it offers cores gives each unit one core
// and an equal part of its memory and storage, so the spec a runtime is given for k cores holds
// k cores and k parts, with no rule of its own for a CPU class.
func TestTheResolvedSpecOfACPUJobHoldsItsCoresAndItsPartOfTheNode(t *testing.T) {
	withCPUClasses(t)
	exp := cpuSubmission(4)
	if err := validate(t, exp); err != nil {
		t.Fatal(err)
	}
	loop := resolveTestLoop()
	total := map[string]map[string]int64{
		"box": {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 64 << 30, domain.NodeResourceStorageBytes: 160 << 30},
	}
	avail := map[string]map[string]int64{"box": {cpuClass: 16}}

	resolved, misfit, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", total, avail, nil, nil)
	if err != nil || misfit != "" {
		t.Fatalf("resolve: misfit = %q, err = %v", misfit, err)
	}
	if resolved.CPU != "4" || resolved.Memory != "16Gi" || resolved.Storage != "40Gi" || resolved.AcceleratorCount != 4 || resolved.AcceleratorType != cpuClass {
		t.Errorf("resolved = cpu %q, memory %q, storage %q, %d of %q; want 4 cores, 16Gi, 40Gi, 4 of the class",
			resolved.CPU, resolved.Memory, resolved.Storage, resolved.AcceleratorCount, resolved.AcceleratorType)
	}
	if exp.Job.Memory != domain.MaxResourceSentinel {
		t.Errorf("the submitted job's memory was rewritten to %q at resolution", exp.Job.Memory)
	}

	// The share bounds a job that names its memory: 4 of 16 cores cannot hold half the node.
	greedy := cpuSubmission(4)
	greedy.Job.Memory = "32Gi"
	if err := validate(t, greedy); err != nil {
		t.Fatal(err)
	}
	if _, misfit, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), greedy, "cluster-a", total, avail, nil, nil); err != nil || !strings.HasPrefix(misfit, domain.NotAdmittedExceedsAcceleratorShare) {
		t.Errorf("4 cores asking for half the node's memory: misfit = %q, err = %v, want it over its share", misfit, err)
	}
}
