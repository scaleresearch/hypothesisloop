package podexec

import (
	"context"
	"testing"
)

// GetClusterID is the bare-metal runtime's answer to autoscaler.md's "Cluster identity": the
// host's /etc/machine-id, trimmed. Every Linux host running this agent has one, so a smoke read
// (rather than mocking the filesystem) is the honest test here.
func TestGetClusterIDReadsMachineID(t *testing.T) {
	e := &Executor{}
	id, err := e.GetClusterID(context.Background())
	if err != nil {
		t.Fatalf("GetClusterID: %v", err)
	}
	if id == "" {
		t.Error("GetClusterID returned empty id — /etc/machine-id should be non-empty on any Linux host")
	}
}
