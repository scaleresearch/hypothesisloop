package podexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClusterIDIsTheTrimmedMachineID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(path, []byte("0123abcd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := readClusterID(path)
	if err != nil || id != "0123abcd" {
		t.Fatalf("readClusterID = %q, %v, want the trimmed id", id, err)
	}
}

func TestAHostWithNoMachineIDFailsAtStartup(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "machine-id")
	if _, err := readClusterID(absent); err == nil {
		t.Error("an absent machine id was accepted")
	}
	empty := filepath.Join(t.TempDir(), "machine-id")
	if err := os.WriteFile(empty, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readClusterID(empty); err == nil {
		t.Error("an empty machine id was accepted")
	}
}
