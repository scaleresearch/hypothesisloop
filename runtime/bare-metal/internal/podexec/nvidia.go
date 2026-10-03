package podexec

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/acceleratorname"
)

// probeNVIDIA discovers local NVIDIA accelerator hardware via NVML (libnvidia-ml.so), the same
// library nvidia-smi itself is a thin CLI wrapper around — no shelling out, no output parsing.
// The label produced here must be byte-identical to GPU Feature Discovery's own normalization, or
// a submitted job's accelerator_type matches nothing and queues forever. A node with no NVIDIA
// driver installed simply reports no accelerators (an empty, valid inventory) rather than
// erroring, since most bare nodes have none.
func probeNVIDIA(_ context.Context) ([]acceleratorDevice, error) {
	switch ret := nvml.Init(); ret {
	case nvml.SUCCESS:
		defer nvml.Shutdown()
	case nvml.ERROR_LIBRARY_NOT_FOUND, nvml.ERROR_DRIVER_NOT_LOADED:
		return nil, nil
	default:
		return nil, fmt.Errorf("podexec: nvml.Init: %v", nvml.ErrorString(ret))
	}

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("podexec: nvml.DeviceGetCount: %v", nvml.ErrorString(ret))
	}
	// One sick GPU must not take the node's whole inventory with it (important.md #19): capacity
	// is probed before desired state is reconciled, so an error here stops deletes as well as
	// admissions. A skipped device under-reports capacity, which can only cost throughput; a
	// failed probe reports none at all.
	devices := make([]acceleratorDevice, 0, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			log.Printf("podexec: skipping NVIDIA device %d: DeviceGetHandleByIndex: %v", i, nvml.ErrorString(ret))
			continue
		}
		name, ret := device.GetName()
		if ret != nvml.SUCCESS {
			log.Printf("podexec: skipping NVIDIA device %d: GetName: %v", i, nvml.ErrorString(ret))
			continue
		}
		// The device node's minor number, not the NVML enumeration index. The two agree on a
		// freshly booted node and diverge after a driver reload or a hot-plug, and occupancy is
		// tracked by /dev path — so booking against the index puts a job's claim on a device it
		// is not using and leaves the one it is using looking free.
		minor, ret := device.GetMinorNumber()
		if ret != nvml.SUCCESS {
			log.Printf("podexec: skipping NVIDIA device %d: GetMinorNumber: %v", i, nvml.ErrorString(ret))
			continue
		}
		devices = append(devices, acceleratorDevice{
			DevicePath: nvidiaDevicePrefix + strconv.Itoa(minor),
			Labels:     []string{acceleratorname.NVIDIAProductLabel(name)},
		})
	}
	return devices, nil
}

// pciDevicesDir lists every PCI device of the host, whether or not a driver is loaded for it.
var pciDevicesDir = "/sys/bus/pci/devices"

// nvidiaCardsOnTheBus counts the NVIDIA display and 3D controllers the kernel enumerated. An
// entry whose vendor cannot be read may be any device of the host and is skipped. An NVIDIA
// device whose class cannot be read is skipped as probeNVIDIA skips a GPU it cannot read. Both
// are logged with their path.
func nvidiaCardsOnTheBus() (int, error) {
	entries, err := os.ReadDir(pciDevicesDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", pciDevicesDir, err)
	}
	cards := 0
	for _, entry := range entries {
		device := filepath.Join(pciDevicesDir, entry.Name())
		vendor, err := os.ReadFile(filepath.Join(device, "vendor"))
		if err != nil {
			log.Printf("podexec: skipping PCI device %s, which cannot be identified: %v", device, err)
			continue
		}
		if strings.TrimSpace(string(vendor)) != "0x10de" {
			continue
		}
		class, err := os.ReadFile(filepath.Join(device, "class"))
		if err != nil {
			log.Printf("podexec: skipping NVIDIA PCI device %s: %v", device, err)
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(string(class)), "0x03") {
			cards++
		}
	}
	return cards, nil
}

// nvidiaPreflight stops a node whose GPUs the probe cannot see. The NVIDIA library missing reads
// as a node without GPUs, which is what most nodes are. A card on the bus says this one has
// them, and it would otherwise report none and take no GPU job with nothing said.
func nvidiaPreflight(devices []acceleratorDevice) ([]string, error) {
	cards, err := nvidiaCardsOnTheBus()
	if err != nil {
		return nil, err
	}
	if cards > 0 && !anyLabelled(devices, "nvidia.com/") {
		return nil, fmt.Errorf("this host has %d NVIDIA card(s) on its PCI bus and the agent reads none of them: libnvidia-ml.so is not loadable where the agent runs, or the NVIDIA driver is not loaded", cards)
	}
	return nil, nil
}

const nvidiaDevicePrefix = "/dev/nvidia"

// nvidiaRequester passes NVIDIA devices as a device request (--gpus), so the
// nvidia-container-toolkit hook injects the host driver libraries CUDA images need. The id of a
// device is the minor number its path ends in; /dev/nvidiactl and its like are no device's path.
var nvidiaRequester = deviceRequester{
	driver:       "nvidia",
	capabilities: [][]string{{"gpu"}},
	id: func(devicePath string) (string, bool) {
		id, ok := strings.CutPrefix(devicePath, nvidiaDevicePrefix)
		return id, ok && id != "" && id[0] >= '0' && id[0] <= '9'
	},
	path: func(id string) string { return nvidiaDevicePrefix + id },
}
