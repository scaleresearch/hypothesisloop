package domain

import "testing"

func TestAJobImageIsAnOfferedNameOrAWholeRegistryAddress(t *testing.T) {
	offered := []JobImage{
		{Name: "python", Image: "registry.test/platform/job:7"},
		{Name: "pytorch-cuda", Image: "registry.test/platform/job-pytorch:7"},
	}
	for stated, want := range map[string]string{
		"python":                           "registry.test/platform/job:7",
		"pytorch-cuda":                     "registry.test/platform/job-pytorch:7",
		"registry.test/platform/job:7":     "registry.test/platform/job:7",
		"ghcr.io/someone/mine@sha256:abcd": "ghcr.io/someone/mine@sha256:abcd",
		"localhost/mine":                   "localhost/mine",
		"registry.test:5000/mine:1":        "registry.test:5000/mine:1",
	} {
		if got, ok := ResolveJobImage(offered, stated); !ok || got != want {
			t.Errorf("ResolveJobImage(%q) = %q, %v, want %q", stated, got, ok, want)
		}
	}
	for _, stated := range []string{"python3", "python:3.13", "pytorch/pytorch:2.4.0", "library/python", "registry.test/", "registry.test/a b", "Python"} {
		if got, ok := ResolveJobImage(offered, stated); ok {
			t.Errorf("ResolveJobImage(%q) = %q, want it refused: it is no offered name and no registry address", stated, got)
		}
	}
}
