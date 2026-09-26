package podexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The engine enforces a container's size on its own store. A node whose scratch directory is on
// XFS said "enforced" while the engine, on another filesystem or without project quotas, held
// no job to anything.
func TestStorageQuotaIsEnforcedOnlyWhereTheEngineEnforcesIt(t *testing.T) {
	store := t.TempDir()
	mounts := func(fsType, options string) string {
		return "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
			"40 22 8:17 / " + store + " rw,relatime shared:5 - " + fsType + " /dev/sdb1 " + options + "\n" +
			"41 22 8:18 / " + store + "-other rw - xfs /dev/sdc1 rw,prjquota\n"
	}
	for name, tc := range map[string]struct {
		driver, storeDir, mountinfo, why string
	}{
		"overlay on XFS with project quotas": {"overlay2", store, mounts("xfs", "rw,attr2,prjquota"), ""},
		"podman's overlay":                   {"overlay", store, mounts("xfs", "rw,pquota"), ""},
		"XFS without project quotas":         {"overlay2", store, mounts("xfs", "rw,attr2,noquota"), "without project quotas"},
		"store on ext4":                      {"overlay2", store, mounts("ext4", "rw"), "is on ext4"},
		"store on the root filesystem":       {"overlay2", store, "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n", "is on ext4"},
		"containerd's snapshotter":           {"overlayfs", store, mounts("xfs", "rw,prjquota"), "storage driver is overlayfs"},
		"a store that is not on this host":   {"overlay", store + "/absent", mounts("xfs", "rw,prjquota"), "not a directory of this host"},
	} {
		path := filepath.Join(t.TempDir(), "mountinfo")
		if err := os.WriteFile(path, []byte(tc.mountinfo), 0o644); err != nil {
			t.Fatal(err)
		}
		enforced, why, err := storageQuota(tc.driver, tc.storeDir, path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if enforced != (tc.why == "") || !strings.Contains(why, tc.why) {
			t.Errorf("%s: enforced = %v, why = %q; want why to say %q", name, enforced, why, tc.why)
		}
	}
}
