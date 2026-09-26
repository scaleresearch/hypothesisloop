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
	tenstorrent := false
	for _, device := range devices {
		for _, label := range device.Labels {
			count[label]++
			tenstorrent = tenstorrent || strings.HasPrefix(label, "tenstorrent.com/")
		}
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
