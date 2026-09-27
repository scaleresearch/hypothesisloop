package podexec

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// totalCPUCores returns this node's logical core count — the bare-node analogue of summing
// k8s Node.Status.Allocatable across schedulable nodes (there is exactly one node here).
var totalCPUCores = func() float64 {
	return float64(runtime.NumCPU())
}

// totalMemoryBytes reads /proc/meminfo's MemTotal, the same source the kernel itself uses to
// report allocatable memory.
func totalMemoryBytes() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("podexec: read /proc/meminfo: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("podexec: malformed /proc/meminfo MemTotal line %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("podexec: parse MemTotal: %w", err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("podexec: MemTotal not found in /proc/meminfo")
}

// engineStore is the engine's store when the agent can measure it. A job's writes land there,
// and the engine enforces a job's size there, so it is what this node reports as its storage.
func engineStore(dir string) (string, error) {
	if stat, err := os.Stat(dir); err != nil || !stat.IsDir() {
		return "", fmt.Errorf("podexec: the engine's store %s is not a directory of this host, so the storage jobs write to cannot be measured; SCRATCH_DIR names a directory to measure in its place", dir)
	}
	return dir, nil
}

// storageFreeBytes statfs's the directory this node's storage is measured on: the engine's
// store, which is where a job's writes land (see Config.StorageDir).
var storageFreeBytes = func(dir string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("podexec: statfs %s: %w", dir, err)
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

// acceleratorDevice is one physical accelerator discovered on this node: its host device path
// (for --device passthrough) and every "key=value" label it should be counted/matched under.
type acceleratorDevice struct {
	DevicePath string
	Labels     []string // e.g. "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"
}

// probeAccelerators discovers every local accelerator this bare node can find, across all
// supported vendors, purely through native libraries/kernel interfaces (nvidia.go: NVML;
// tenstorrent.go: tt-kmd's sysfs class) — never by shelling out to a vendor CLI. A node with
// neither driver present simply reports no accelerators (an empty, valid inventory) rather than
// erroring, since most bare nodes have none; a CPU-only job never calls this at all
// (resolvePlacementFor short-circuits when AcceleratorCount <= 0).
func probeAccelerators(ctx context.Context) ([]acceleratorDevice, error) {
	var devices []acceleratorDevice

	nvidiaDevices, err := probeNVIDIA(ctx)
	if err != nil {
		return nil, err
	}
	devices = append(devices, nvidiaDevices...)

	tenstorrentDevices, err := probeTenstorrent(ctx)
	if err != nil {
		return nil, err
	}
	devices = append(devices, tenstorrentDevices...)

	return devices, nil
}
