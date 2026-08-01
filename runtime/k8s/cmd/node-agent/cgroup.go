package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// readAllPodStats walks cgroupRoot looking for cpu.stat files and returns a map
// of cgroup path → entry. Pod UID is parsed from the path.
func readAllPodStats() map[string]cpuStatEntry {
	result := map[string]cpuStatEntry{}
	_ = filepath.WalkDir(cgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "cpu.stat" {
			return nil
		}
		usage, ok := readUsageUsec(path)
		if !ok {
			return nil
		}
		uid := extractPodUID(path)
		result[path] = cpuStatEntry{
			path:      path,
			podUID:    uid,
			usageUsec: usage,
			readAt:    time.Now(),
		}
		return nil
	})
	return result
}

// readUsageUsec extracts the usage_usec value from a cgroup v2 cpu.stat file.
func readUsageUsec(path string) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "usage_usec ") {
			parts := strings.Fields(line)
			if len(parts) == 2 {
				v, err := strconv.ParseUint(parts[1], 10, 64)
				if err == nil {
					return v, true
				}
			}
		}
	}
	return 0, false
}

// extractPodUID parses a pod UID from a cgroup path. Two layouts are supported:
//
//   - plain cgroupfs:            .../pod<uid-with-hyphens>/...
//   - systemd cgroup-v2 slices:  .../kubepods[-<qos>]-pod<uid_with_underscores>.slice/...
//
// In the systemd layout the UID's hyphens are escaped as underscores and the
// component carries a "kubepods[-<qos>]-" prefix plus a ".slice" suffix, so a
// plain HasPrefix(part, "pod") check never matches it.
func extractPodUID(path string) string {
	parts := strings.Split(path, string(os.PathSeparator))
	for _, part := range parts {
		if uid := podUIDFromPathComponent(part); uid != "" {
			return uid
		}
	}
	return ""
}

// podUIDFromPathComponent extracts a pod UID from a single cgroup path component,
// or returns "" if the component doesn't identify a pod.
func podUIDFromPathComponent(part string) string {
	part = strings.TrimSuffix(part, ".slice")
	// The last "pod" occurrence is the one immediately preceding the UID: the
	// systemd layout also contains "pod" earlier, inside the "kubepods" prefix.
	idx := strings.LastIndex(part, "pod")
	if idx == -1 {
		return ""
	}
	uid := part[idx+len("pod"):]
	// Convert underscores to hyphens (systemd cgroup-v2 escaping); plain
	// cgroupfs UIDs already use hyphens, so this is a no-op for that layout.
	uid = strings.ReplaceAll(uid, "_", "-")
	if isValidUID(uid) {
		return uid
	}
	return ""
}

// isValidUID does a basic check for Kubernetes UID format (8-4-4-4-12).
func isValidUID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	lens := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != lens[i] {
			return false
		}
	}
	return true
}
