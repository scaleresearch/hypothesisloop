package command

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
)

// LocalOnly reports whether address, a URL or a registry's host[:port][/path], names a host only
// the platform's own machine reaches: a loopback address, or the name a container engine gives
// its host to the containers it runs.
func LocalOnly(address string) bool {
	if !strings.Contains(address, "://") {
		address = "//" + address
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "localhost", "host.containers.internal", "host.docker.internal", "0.0.0.0", "":
		return host != ""
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// WarnLocalOnly says on stderr that what, at address, cannot be reached from another machine.
func WarnLocalOnly(stderr io.Writer, what, address string) {
	if LocalOnly(address) {
		fmt.Fprintf(stderr, "hl: warning: %s %s is reached only from the platform's own machine: other machines cannot reach it. "+
			"On a local stack, set LOCAL_REACHABLE_ADDRESS in .env (.env.example) and run make dev again\n", what, address)
	}
}
