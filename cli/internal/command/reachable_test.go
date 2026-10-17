package command

import (
	"testing"
)

func TestLocalOnlyNamesTheAddressesNoOtherMachineReaches(t *testing.T) {
	for address, want := range map[string]bool{
		"http://host.containers.internal:8081": true, "host.containers.internal:5000/charts": true, "http://localhost:8084": true,
		"127.0.0.1:5000": true, "http://[::1]:8081": true, "http://0.0.0.0:9000": true, "HOST.DOCKER.INTERNAL:5000": true,
		"http://100.64.0.5:8081": false, "100.64.0.5:5000": false, "registry.example/images": false, "https://api.example": false, "": false,
	} {
		if got := LocalOnly(address); got != want {
			t.Errorf("localOnly(%q) = %v, want %v", address, got, want)
		}
	}
}
