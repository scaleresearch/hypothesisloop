package podexec

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tenstorrentClassDir is the tt-kmd driver's sysfs class directory: one entry per chip, named
// "tenstorrent!N", symlinked to that chip's backing PCI device directory.
var tenstorrentClassDir = "/sys/class/tenstorrent"

// tenstorrentChipArch maps a Tenstorrent PCI device ID — the kernel's own "device" sysfs
// attribute, not anything we compute — to the chip architecture name tt-metal/ttnn and this
// platform's DRA catalog both call it by (tenstorrent.com/chipArch, see
// controlplane/shared/domain/accelerator.go). Same role as acceleratorname.NVIDIAProductLabel plays for
// NVIDIA: reproducing the vendor's own naming exactly so a submitted accelerator_type matches.
var tenstorrentChipArch = map[string]string{
	"0xfaca": "grayskull",
	"0x401e": "wormhole",
	"0xb140": "blackhole",
	"0xb150": "blackhole",
}

// probeTenstorrent discovers local Tenstorrent ASICs purely through the tt-kmd driver's sysfs
// class — no ioctl, no shelling out to tt-smi. Each chip publishes itself as
// /sys/class/tenstorrent/tenstorrent!N, a symlink to its PCI device directory, whose "device"
// attribute is the PCI device ID the kernel itself read off the card. A node with no tt-kmd
// loaded simply has no such class directory — reported as an empty, valid inventory, same as
// probeNVIDIA finding no NVML library.
func probeTenstorrent(_ context.Context) ([]acceleratorDevice, error) {
	entries, err := os.ReadDir(tenstorrentClassDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("podexec: read %s: %w", tenstorrentClassDir, err)
	}

	// One chip that cannot be read is skipped and the rest reported, as probeNVIDIA does and for
	// the same reason: a failed probe stops every pass on the node, deletes included.
	devices := make([]acceleratorDevice, 0, len(entries))
	for _, entry := range entries {
		index := strings.TrimPrefix(entry.Name(), "tenstorrent!")
		if index == entry.Name() {
			continue
		}
		if _, err := strconv.Atoi(index); err != nil {
			continue
		}

		classLink := filepath.Join(tenstorrentClassDir, entry.Name())
		chipDir, err := filepath.EvalSymlinks(classLink)
		if err != nil {
			log.Printf("podexec: skipping Tenstorrent chip %s: resolve %s: %v", index, classLink, err)
			continue
		}
		// chipDir is .../<pci-addr>/tenstorrent/tenstorrent!N; the PCI device directory itself
		// (carrying the "device" attribute) is two levels up.
		pciDeviceDir := filepath.Dir(filepath.Dir(chipDir))
		raw, err := os.ReadFile(filepath.Join(pciDeviceDir, "device"))
		if err != nil {
			log.Printf("podexec: skipping Tenstorrent chip %s: %v", index, err)
			continue
		}
		deviceID := strings.ToLower(strings.TrimSpace(string(raw)))
		arch, known := tenstorrentChipArch[deviceID]
		if !known {
			log.Printf("podexec: skipping Tenstorrent chip %s at %s: unrecognized PCI device id %q", index, pciDeviceDir, deviceID)
			continue
		}

		devices = append(devices, acceleratorDevice{
			DevicePath: filepath.Join("/dev/tenstorrent", index),
			Labels:     []string{tenstorrentLabelPrefix + "chipArch=" + arch},
		})
	}
	return devices, nil
}

const tenstorrentLabelPrefix = "tenstorrent.com/"

// tenstorrentPreflight says how many 1 GiB hugepages a node with a chip has reserved: the
// platform gives every job on a chip hugepages-1Gi, which its driver maps the chip through.
func tenstorrentPreflight(devices []acceleratorDevice) ([]string, error) {
	if !anyLabelled(devices, tenstorrentLabelPrefix) {
		return nil, nil
	}
	reserved, err := hugepageSizes["hugepages-1Gi"].reservedPages()
	if err != nil {
		return []string{"hugepages: cannot read how many 1 GiB hugepages are reserved on this host: " + err.Error()}, nil
	}
	if reserved == 0 {
		return []string{"hugepages: no 1 GiB hugepages are reserved on this host, and a job that asks for hugepages-1Gi is refused as one this node cannot run"}, nil
	}
	return []string{fmt.Sprintf("hugepages: %d x 1 GiB reserved", reserved)}, nil
}
