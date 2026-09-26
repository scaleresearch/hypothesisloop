package podexec

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

// gigabyteHugepages is where the kernel says how many 1 GiB hugepages the host reserved.
var gigabyteHugepages = "/sys/kernel/mm/hugepages/hugepages-1048576kB/nr_hugepages"

// pciDevicesDir lists every PCI device of the host, whether or not a driver is loaded for it.
var pciDevicesDir = "/sys/bus/pci/devices"

// nvidiaCardsOnTheBus counts the NVIDIA display and 3D controllers the kernel enumerated.
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
		vendor, _ := os.ReadFile(pciDevicesDir + "/" + entry.Name() + "/vendor")
		class, _ := os.ReadFile(pciDevicesDir + "/" + entry.Name() + "/class")
		if strings.TrimSpace(string(vendor)) == "0x10de" && strings.HasPrefix(strings.TrimSpace(string(class)), "0x03") {
			cards++
		}
	}
	return cards, nil
}

// Preflight states what this node offers before the agent takes any work, one line a fact, and
// fails on what makes every job fail: a control plane that does not answer from here. The engine
// and the scratch directory were already required by New.
func (e *Executor) Preflight(ctx context.Context, client *http.Client) ([]string, error) {
	health := strings.TrimRight(e.apiURL, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health, nil)
	if err != nil {
		return nil, fmt.Errorf("API_URL %s is not a URL: %w", e.apiURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the control plane does not answer at %s from this node: %w", health, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d, so API_URL %s is not the control plane's API", health, resp.StatusCode, e.apiURL)
	}
	lines := []string{
		"control plane: " + e.apiURL + " answers",
		"container engine: " + e.engineName,
		"node: " + e.nodeName + ", cluster id " + e.clusterID,
	}

	devices, err := e.probe(ctx)
	if err != nil {
		return nil, fmt.Errorf("read this node's accelerators: %w", err)
	}
	count := map[string]int{}
	tenstorrent, nvidia := false, false
	for _, device := range devices {
		for _, label := range device.Labels {
			count[label]++
			tenstorrent = tenstorrent || strings.HasPrefix(label, "tenstorrent.com/")
			nvidia = nvidia || strings.HasPrefix(label, "nvidia.com/")
		}
	}
	// The NVIDIA library missing reads as a node without GPUs, which is what most nodes are. A
	// card on the bus says this one has them, and it would otherwise report none and take no
	// GPU job with nothing said.
	cards, err := nvidiaCardsOnTheBus()
	if err != nil {
		return nil, err
	}
	if cards > 0 && !nvidia {
		return nil, fmt.Errorf("this host has %d NVIDIA card(s) on its PCI bus and the agent reads none of them: libnvidia-ml.so is not loadable where the agent runs, or the NVIDIA driver is not loaded", cards)
	}
	if len(count) == 0 {
		lines = append(lines, "accelerators: none found, so this node takes jobs that ask for none")
	}
	labels := make([]string, 0, len(count))
	for label := range count {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		lines = append(lines, fmt.Sprintf("accelerators: %d x %s", count[label], label))
	}
	if tenstorrent {
		lines = append(lines, hugepagesLine())
	}

	if e.storageQuotaEnforced {
		lines = append(lines, "job storage: "+e.scratchDir+", each job held to the storage it asked for")
	} else {
		lines = append(lines, "job storage: "+e.scratchDir+" is not on XFS, so a job's storage request is not enforced here")
	}
	return lines, nil
}

func hugepagesLine() string {
	raw, err := os.ReadFile(gigabyteHugepages)
	if err != nil {
		return "hugepages: cannot read " + gigabyteHugepages + ": " + err.Error()
	}
	reserved, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || reserved == 0 {
		return "hugepages: no 1 GiB hugepages are reserved on this host, and a job that asks for hugepages-1Gi on a Tenstorrent chip will not start"
	}
	return fmt.Sprintf("hugepages: %d x 1 GiB reserved", reserved)
}
