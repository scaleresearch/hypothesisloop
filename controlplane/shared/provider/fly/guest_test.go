package fly

import (
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
)

func TestGuestIsTheSmallestMachineThatHoldsTheSpec(t *testing.T) {
	cases := map[string]struct {
		kind     string
		millis   int64
		memory   int64
		cpus, mb int64
	}{
		"the agent shape":                     {"shared", 2000, 4 << 30, 2, 4096},
		"a fraction of a core":                {"shared", 250, 256 << 20, 1, 256},
		"memory rounded up to the step":       {"shared", 1000, 1<<30 + 1, 1, 1280},
		"cores rounded up to a size sold":     {"shared", 2001, 1 << 30, 4, 1024},
		"five cores are six":                  {"shared", 5000, 4 << 30, 6, 4096},
		"memory raised to what cores bring":   {"shared", 8000, 1 << 30, 8, 2048},
		"more cores to hold the memory":       {"shared", 1000, 3 << 30, 2, 3072},
		"the largest shared machine":          {"shared", 8000, 16 << 30, 8, 16384},
		"performance has two GB a core":       {"performance", 1000, 1 << 30, 1, 2048},
		"performance steps by one GB":         {"performance", 2000, 4<<30 + 1, 2, 5120},
		"performance cores for the memory":    {"performance", 1000, 9 << 30, 2, 9216},
		"the largest performance machine":     {"performance", 16000, 128 << 30, 16, 131072},
		"performance cores between the sizes": {"performance", 9000, 20 << 30, 10, 20480},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Settings{CPUKind: tc.kind}.guestFor(provider.Spec{Name: "hl-a", CPUMillis: tc.millis, MemoryBytes: tc.memory})
			if err != nil {
				t.Fatalf("guestFor: %v", err)
			}
			if got != (guest{CPUKind: tc.kind, CPUs: tc.cpus, MemoryMB: tc.mb}) {
				t.Errorf("guest = %+v, want %d cores and %d MB", got, tc.cpus, tc.mb)
			}
		})
	}
}

func TestGuestRefusesWhatNoMachineHolds(t *testing.T) {
	cases := map[string]struct {
		kind   string
		millis int64
		memory int64
		want   string
	}{
		"too many shared cores":      {"shared", 8001, 1 << 30, "8 cores"},
		"too much shared memory":     {"shared", 1000, 16<<30 + 1, "16384 MB"},
		"too many performance cores": {"performance", 17000, 1 << 30, "16 cores"},
		"too much performance mem":   {"performance", 1000, 129 << 30, "131072 MB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Settings{CPUKind: tc.kind}.guestFor(provider.Spec{Name: "hl-a", CPUMillis: tc.millis, MemoryBytes: tc.memory})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "hl-a") {
				t.Fatalf("err = %v, want one naming the spec and the largest machine (%s)", err, tc.want)
			}
		})
	}
}

func TestRootfsIsTheDiskRoundedUpToAGigabyte(t *testing.T) {
	for bytes, want := range map[int64]int64{1: 1, 8 << 30: 8, 8<<30 + 1: 9, 50 << 30: 50} {
		got, err := rootfsGB(provider.Spec{DiskBytes: bytes})
		if err != nil || got != want {
			t.Errorf("rootfsGB(%d) = %d, %v, want %d", bytes, got, err, want)
		}
	}
	_, err := rootfsGB(provider.Spec{Name: "hl-a", DiskBytes: 50<<30 + 1})
	if err == nil || !strings.Contains(err.Error(), "at most 50 GB") {
		t.Fatalf("err = %v, want a refusal of a disk the root filesystem cannot hold", err)
	}
}
