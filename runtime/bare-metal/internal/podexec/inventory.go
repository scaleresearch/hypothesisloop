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
func totalCPUCores() float64 {
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

// storageQuota says whether the engine holds a container to a storage-opt size, and why not
// when it does not. Both engines enforce it in one case only: the overlay driver, with its
// store on XFS mounted with project quotas. The store is the engine's own directory, wherever
// the node's scratch directory is, so it is the store's mount that is read, from mountinfoPath
// (/proc/self/mountinfo). An engine whose store is not a directory of this host (a remote
// one) is one this agent cannot vouch for.
func storageQuota(driver, storeDir, mountinfoPath string) (enforced bool, why string, err error) {
	if driver != "overlay" && driver != "overlay2" {
		return false, fmt.Sprintf("the engine's storage driver is %s, which holds no container to a size", driver), nil
	}
	if info, err := os.Stat(storeDir); err != nil || !info.IsDir() {
		return false, fmt.Sprintf("the engine's store %s is not a directory of this host", storeDir), nil
	}
	mountinfo, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return false, "", fmt.Errorf("podexec: read this host's mounts: %w", err)
	}
	mountPoint, fsType, options := "", "", ""
	for _, line := range strings.Split(string(mountinfo), "\n") {
		before, after, found := strings.Cut(line, " - ")
		fields, super := strings.Fields(before), strings.Fields(after)
		if !found || len(fields) < 5 || len(super) < 3 {
			continue
		}
		point := fields[4]
		under := point == "/" || storeDir == point || strings.HasPrefix(storeDir, point+"/")
		if under && len(point) >= len(mountPoint) {
			mountPoint, fsType, options = point, super[0], super[2]
		}
	}
	if fsType != "xfs" {
		return false, fmt.Sprintf("the engine's store %s is on %s, and only XFS holds a container to a size", storeDir, fsType), nil
	}
	for _, option := range strings.Split(options, ",") {
		if option == "prjquota" || option == "pquota" {
			return true, "", nil
		}
	}
	return false, fmt.Sprintf("the engine's store %s is on XFS mounted at %s without project quotas (the pquota mount option)", storeDir, mountPoint), nil
}

// scratchCapacityBytes statfs's the configured scratch directory, which is what this node
// reports as its storage.
func scratchCapacityBytes(dir string) (total, available int64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, 0, fmt.Errorf("podexec: statfs %s: %w", dir, err)
	}
	total = int64(stat.Blocks) * int64(stat.Bsize)
	available = int64(stat.Bavail) * int64(stat.Bsize)
	return total, available, nil
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
