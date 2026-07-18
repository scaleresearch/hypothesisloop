package main

import "testing"

func TestExtractPodUID(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{
			name: "systemd cgroup-v2 burstable slice",
			path: "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789012.slice/crio-abcdef.scope/cpu.stat",
			want: "12345678-1234-1234-1234-123456789012",
		},
		{
			name: "systemd cgroup-v2 plain kubepods slice (no qos)",
			path: "/sys/fs/cgroup/kubepods.slice/kubepods-pod87654321_4321_4321_4321_210987654321.slice/crio-abcdef.scope/cpu.stat",
			want: "87654321-4321-4321-4321-210987654321",
		},
		{
			name: "plain cgroupfs layout",
			path: "/sys/fs/cgroup/kubepods/burstable/pod12345678-1234-1234-1234-123456789012/abcdef/cpu.stat",
			want: "12345678-1234-1234-1234-123456789012",
		},
		{
			name: "no pod component",
			path: "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/cpu.stat",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPodUID(tc.path)
			if got != tc.want {
				t.Errorf("extractPodUID(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
