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

	"github.com/moby/moby/api/types/container"
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

// anyLabelled says whether one of devices carries a label of the vendor whose labels start with
// prefix.
func anyLabelled(devices []acceleratorDevice, prefix string) bool {
	for _, device := range devices {
		for _, label := range device.Labels {
			if strings.HasPrefix(label, prefix) {
				return true
			}
		}
	}
	return false
}

// deviceRequester is a vendor whose devices reach a container as an engine device request in
// place of a plain device mapping. id reads the request's device id off a host device path the
// vendor's probe returned, with ok false for a path that is not the vendor's, and path reverses it.
type deviceRequester struct {
	driver       string
	capabilities [][]string
	id           func(devicePath string) (id string, ok bool)
	path         func(id string) string
}

var deviceRequesters = []deviceRequester{nvidiaRequester}

// passDevices is how host device paths are handed to a container: one device request per vendor
// that needs one, a plain mapping for every other path.
func passDevices(devicePaths []string) ([]container.DeviceMapping, []container.DeviceRequest) {
	mappings := make([]container.DeviceMapping, 0, len(devicePaths))
	ids := map[string][]string{}
next:
	for _, d := range devicePaths {
		for _, r := range deviceRequesters {
			if id, ok := r.id(d); ok {
				ids[r.driver] = append(ids[r.driver], id)
				continue next
			}
		}
		mappings = append(mappings, container.DeviceMapping{PathOnHost: d, PathInContainer: d, CgroupPermissions: "rwm"})
	}
	var requests []container.DeviceRequest
	for _, r := range deviceRequesters {
		if len(ids[r.driver]) > 0 {
			requests = append(requests, container.DeviceRequest{Driver: r.driver, DeviceIDs: ids[r.driver], Capabilities: r.capabilities})
		}
	}
	return mappings, requests
}

// devicesPassed reverses passDevices: the host device paths a container was created with.
// Reading the mappings alone made every device passed by request look free, and placement handed
// the same one to job after job.
func devicesPassed(host *container.HostConfig) []string {
	var devicePaths []string
	for _, d := range host.Devices {
		devicePaths = append(devicePaths, d.PathOnHost)
	}
	for _, req := range host.DeviceRequests {
		for _, r := range deviceRequesters {
			if r.driver != req.Driver {
				continue
			}
			for _, id := range req.DeviceIDs {
				devicePaths = append(devicePaths, r.path(id))
			}
		}
	}
	return devicePaths
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
