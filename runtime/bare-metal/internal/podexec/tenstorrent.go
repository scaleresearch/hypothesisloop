package podexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tenstorrentClassDir is the tt-kmd driver's sysfs class directory: one entry per chip, named
// "tenstorrent!N", symlinked to that chip's backing PCI device directory.
const tenstorrentClassDir = "/sys/class/tenstorrent"

// tenstorrentChipArch maps a Tenstorrent PCI device ID — the kernel's own "device" sysfs
// attribute, not anything we compute — to the chip architecture name tt-metal/ttnn and this
// platform's DRA catalog both call it by (tenstorrent.com/chipArch, see
// controlplane/shared/domain/accelerator.go). Same role as normalizeGFDProductName plays for
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
			return nil, fmt.Errorf("podexec: resolve %s: %w", classLink, err)
		}
		// chipDir is .../<pci-addr>/tenstorrent/tenstorrent!N; the PCI device directory itself
		// (carrying the "device" attribute) is two levels up.
		pciDeviceDir := filepath.Dir(filepath.Dir(chipDir))
		raw, err := os.ReadFile(filepath.Join(pciDeviceDir, "device"))
		if err != nil {
			return nil, fmt.Errorf("podexec: read %s: %w", filepath.Join(pciDeviceDir, "device"), err)
		}
		deviceID := strings.ToLower(strings.TrimSpace(string(raw)))
		arch, known := tenstorrentChipArch[deviceID]
		if !known {
			return nil, fmt.Errorf("podexec: tenstorrent chip at %s has unrecognized PCI device id %q", pciDeviceDir, deviceID)
		}

		devices = append(devices, acceleratorDevice{
			DevicePath: filepath.Join("/dev/tenstorrent", index),
			Labels:     []string{"tenstorrent.com/chipArch=" + arch},
		})
	}
	return devices, nil
}
