package scheduler

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// One type's unit is a CPU core (domain.CPUType). These tests hold what that changes at
// submission and in the spec a runtime is given, and that a submission for an accelerator reads
// as it did before the catalog priced cores.

const cpuType = domain.CPUType

// withCPUType prices cores beside h100 and l40 for one test, at the rate the shipped catalog
// gives them. The catalog is process-wide, so it is put back as the package's init left it.
func withCPUType(t *testing.T) {
	t.Helper()
	domain.SetAcceleratorRates(map[string]float64{h100: 1.0, l40: 0.25, string(cpuType): 1.0})
	t.Cleanup(func() { domain.SetAcceleratorRates(map[string]float64{h100: 1.0}) })
}

// cpuSubmission asks for cores the plain way.
func cpuSubmission(cores int) *domain.Experiment {
	exp := validSubmission()
	exp.AcceleratorType, exp.AcceleratorCount = "", 0
	exp.Job.AcceleratorType, exp.Job.AcceleratorCount = "", 0
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = strconv.Itoa(cores), "1Gi", "1Gi"
	return exp
}

func TestAJobOnCPUHoldsItsCoresAsUnitsOfTheCPUType(t *testing.T) {
	withCPUType(t)
	type job struct {
		typ                  domain.AcceleratorType
		count                int
		cpu, memory, storage string
	}
	for name, tc := range map[string]struct {
		in, want job
		refused  string
	}{
		"cores alone":                {in: job{cpu: "4", memory: "2Gi", storage: "1Gi"}, want: job{cpuType, 4, "4", "2Gi", "1Gi"}},
		"the type and cores":         {in: job{typ: cpuType, cpu: "4", memory: "2Gi", storage: "1Gi"}, want: job{cpuType, 4, "4", "2Gi", "1Gi"}},
		"the type, cores and count":  {in: job{typ: cpuType, count: 8, cpu: "8000m", memory: "4Gi", storage: "20Gi"}, want: job{cpuType, 8, "8000m", "4Gi", "20Gi"}},
		"half a core":                {in: job{cpu: "500m", memory: "2Gi", storage: "1Gi"}, refused: "whole cores"},
		"a core and a half":          {in: job{cpu: "1.5", memory: "2Gi", storage: "1Gi"}, refused: "whole cores"},
		"no cores":                   {in: job{cpu: "0", memory: "2Gi", storage: "1Gi"}, refused: "whole cores"},
		"cores other than the count": {in: job{typ: cpuType, count: 8, cpu: "4", memory: "2Gi", storage: "1Gi"}, refused: "leave the count out or make them equal"},
		"the type and a count":       {in: job{typ: cpuType, count: 4, memory: "2Gi", storage: "1Gi"}, refused: "job.cpu is required"},
		"the type and nothing":       {in: job{typ: cpuType}, refused: "job.cpu is required"},
	} {
		exp := cpuSubmission(1)
		exp.Job.AcceleratorType, exp.Job.AcceleratorCount = tc.in.typ, tc.in.count
		exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = tc.in.cpu, tc.in.memory, tc.in.storage
		exp.AcceleratorCount = tc.in.count
		err := validate(t, exp)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%s: err = %v, want a refusal saying %q", name, err, tc.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := (job{exp.Job.AcceleratorType, exp.Job.AcceleratorCount, exp.Job.CPU, exp.Job.Memory, exp.Job.Storage}); got != tc.want {
			t.Errorf("%s: job = %+v, want %+v", name, got, tc.want)
		}
		if exp.AcceleratorType != tc.want.typ || exp.AcceleratorCount != tc.want.count {
			t.Errorf("%s: admitted as %d of %q, want %d of %q", name, exp.AcceleratorCount, exp.AcceleratorType, tc.want.count, tc.want.typ)
		}
	}

	// The cores of a job are bounded by the cores limit and by no limit on devices, and the
	// refusal names the field the caller wrote.
	caps := testCaps
	caps.MaxAcceleratorCountPerJob, caps.MaxCPUCoresPerJob = 16, 64
	for cores, refused := range map[string]string{"24": "", "80": "job.cpu 80 cores (x1 nodes) is over the most cores one job may ask for, 64"} {
		exp := cpuSubmission(1)
		exp.Job.AcceleratorType, exp.Job.AcceleratorCount, exp.AcceleratorCount, exp.Job.CPU = "", 0, 0, cores
		if err := ValidateExperiment(exp, caps); (err == nil) != (refused == "") || (err != nil && !strings.Contains(err.Error(), refused)) {
			t.Errorf("cpu: %s under a limit of 64 cores and 16 devices: err = %v, want %q", cores, err, refused)
		}
	}

	// A job runs on cores or on devices: one count cannot be both.
	exp := cpuSubmission(4)
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{h100}
	mustReject(t, exp, "never either")
	exp = validSubmission()
	exp.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{cpuType}
	mustReject(t, exp, "never either")
}

func TestEachGroupOfAJobOnCPUHoldsItsOwnCores(t *testing.T) {
	withCPUType(t)
	exp := cpuSubmission(1)
	exp.Job.CPU, exp.Job.Memory, exp.Job.Storage = "", "", ""
	exp.Job.Groups = []domain.JobGroup{
		{Name: "driver", Replicas: 1, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
		{Name: "workers", Replicas: 3, CPU: "4", Memory: "2Gi", Storage: "1Gi"},
	}
	if err := validate(t, exp); err != nil {
		t.Fatalf("a grouped job on cores: %v", err)
	}
	want := []domain.JobGroup{
		{Name: "driver", Replicas: 1, AcceleratorCount: 2, CPU: "2", Memory: "1Gi", Storage: "1Gi"},
		{Name: "workers", Replicas: 3, AcceleratorCount: 4, CPU: "4", Memory: "2Gi", Storage: "1Gi"},
	}
	if !reflect.DeepEqual(exp.Job.Groups, want) {
		t.Errorf("groups = %+v, want %+v", exp.Job.Groups, want)
	}
	if exp.AcceleratorCount != 14 || exp.AcceleratorType != cpuType {
		t.Errorf("admitted as %d of %q, want the 14 cores of all replicas", exp.AcceleratorCount, exp.AcceleratorType)
	}

	// A group that states no cores would run beside the groups that pay.
	exp.Job.Groups = []domain.JobGroup{{Name: "driver", Replicas: 1, CPU: "2", Memory: "1Gi", Storage: "1Gi"}, {Name: "free", Replicas: 8, Memory: "2Gi", Storage: "1Gi"}}
	exp.Job.AcceleratorType = cpuType
	if err := validate(t, exp); err == nil || !strings.Contains(err.Error(), `job.groups["free"].cpu is required`) {
		t.Errorf("a group that states no cores: err = %v", err)
	}
}

// The same submissions, with the catalog pricing cores and without: nothing about an accelerator
// job is read differently.
func TestAnAcceleratorSubmissionIsValidatedAsBeforeBesideTheCPUType(t *testing.T) {
	submissions := map[string]func() *domain.Experiment{
		"plain": validSubmission,
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
	withCPUType(t)
	if after := read(); !reflect.DeepEqual(before, after) {
		t.Errorf("accelerator submissions read differently once the catalog prices cores:\nbefore %+v\nafter  %+v", before, after)
	}
	if !strings.Contains(before["no cpu"].err, ReasonRequired) || before["plain"].err != "" {
		t.Errorf("the comparison proves nothing: %+v", before)
	}
}

// Each core of a node comes with an equal part of its memory and storage, measured against the
// cores the node has and never against the ones free, so the cores an accelerator job holds
// beside a CPU job do not allow the CPU job that job's memory. A job holds what it stated.
func TestTheResolvedSpecOfAJobOnCPUIsWhatItStatedWithinItsCoresPartOfTheNode(t *testing.T) {
	withCPUType(t)
	loop := resolveTestLoop()
	total := map[string]map[string]int64{
		"box": {domain.NodeResourceCPUMillicores: 16000, domain.NodeResourceMemoryBytes: 64 << 30, domain.NodeResourceStorageBytes: 160 << 30},
	}
	resolve := func(exp *domain.Experiment, freeCores int64) (*domain.JobSpec, string) {
		t.Helper()
		if err := validate(t, exp); err != nil {
			t.Fatal(err)
		}
		avail := map[string]map[string]int64{"box": {string(cpuType): freeCores, "tenstorrent.com/chipArch=blackhole": 3}}
		resolved, misfit, err := loop.resolveClusterLocalResources(context.Background(), newResolutionCache(loop), exp, "cluster-a", total, avail, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return resolved, misfit
	}

	for name, freeCores := range map[string]int64{"an idle node": 16, "a chip job holds 4 cores": 12, "no core free": 0} {
		exp := cpuSubmission(4)
		exp.Job.Memory = "16Gi"
		resolved, misfit := resolve(exp, freeCores)
		if misfit != "" {
			t.Fatalf("%s: misfit = %q", name, misfit)
		}
		if resolved.CPU != "4" || resolved.Memory != "16Gi" || resolved.Storage != "1Gi" || resolved.AcceleratorCount != 4 || resolved.AcceleratorType != cpuType {
			t.Errorf("%s: resolved = cpu %q, memory %q, storage %q, %d of %q; want the 4 cores, 16Gi and 1Gi stated, 4 of the CPU type",
				name, resolved.CPU, resolved.Memory, resolved.Storage, resolved.AcceleratorCount, resolved.AcceleratorType)
		}
	}

	// The share bounds a job that names its memory: 4 of 16 cores cannot hold half the node.
	greedy := cpuSubmission(4)
	greedy.Job.Memory = "32Gi"
	if _, misfit := resolve(greedy, 16); !strings.HasPrefix(misfit, domain.NotAdmittedExceedsAcceleratorShare) {
		t.Errorf("4 cores asking for half the node's memory: misfit = %q, want it over its share", misfit)
	}
}
