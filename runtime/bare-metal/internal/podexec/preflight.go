package podexec

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// vendorPreflights is what each vendor adds once the node's devices are read: lines to say of
// its own devices, or an error for what the probe cannot have seen.
var vendorPreflights = []func(devices []acceleratorDevice) ([]string, error){nvidiaPreflight, tenstorrentPreflight}

// Preflight states what this node offers before the agent takes any work, one line a fact, and
// fails on what makes every job fail: a control plane that does not answer from here. The engine
// and its store were already required by New.
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
	for _, device := range devices {
		for _, label := range device.Labels {
			count[label]++
		}
	}
	var vendorLines []string
	for _, preflight := range vendorPreflights {
		said, err := preflight(devices)
		if err != nil {
			return nil, err
		}
		vendorLines = append(vendorLines, said...)
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
	lines = append(lines, vendorLines...)

	lines = append(lines, "job storage: measured on "+e.storageDir)
	if e.storageQuotaEnforced {
		lines = append(lines, "job storage: the engine holds each job to the storage it asked for")
	} else {
		lines = append(lines, "job storage: a job's storage request is counted and not enforced here: "+e.storageQuotaRefusal)
	}
	return lines, nil
}
