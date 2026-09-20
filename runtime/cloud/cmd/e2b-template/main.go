// e2b-template builds the E2B template the e2b adapter starts a sandbox of an image from at one
// size (e2b.TemplateBuilder). It reads E2B_API_KEY and, for an image in a private registry,
// E2B_REGISTRY_USERNAME and E2B_REGISTRY_PASSWORD.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/e2b"
)

func main() {
	const usage = "usage: e2b-template <image> <cores> <memory-mib>"
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	cores, coresErr := strconv.ParseInt(os.Args[2], 10, 64)
	memory, memoryErr := strconv.ParseInt(os.Args[3], 10, 64)
	if coresErr != nil || memoryErr != nil || cores <= 0 || memory <= 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	builder := e2b.TemplateBuilder{
		APIKey:           os.Getenv("E2B_API_KEY"),
		RegistryUsername: os.Getenv("E2B_REGISTRY_USERNAME"),
		RegistryPassword: os.Getenv("E2B_REGISTRY_PASSWORD"),
		Out:              os.Stdout,
	}
	if err := builder.Build(os.Args[1], cores, memory); err != nil {
		fmt.Fprintf(os.Stderr, "e2b-template: %v\n", err)
		os.Exit(1)
	}
}
