// e2b-template builds the E2B template the e2b adapter starts a sandbox of an image from at one
// size (e2b.TemplateBuilder), and with prune deletes the templates of every other image tag. It
// reads E2B_API_KEY and, for an image in a private registry, E2B_REGISTRY_USERNAME and
// E2B_REGISTRY_PASSWORD.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/e2b"
)

const usage = `usage: e2b-template <image> <cores> <memory-mib>
       e2b-template prune <cores> <memory-mib> <image>...   deletes every template built here but those of the images at this size`

func main() {
	args := os.Args[1:]
	prune := len(args) > 0 && args[0] == "prune"
	var images, sizeArgs []string
	switch {
	case prune && len(args) >= 4:
		sizeArgs, images = args[1:3], args[3:]
	case !prune && len(args) == 3:
		images, sizeArgs = args[:1], args[1:]
	default:
		refuse(usage)
	}
	cores, coresErr := strconv.ParseInt(sizeArgs[0], 10, 64)
	memory, memoryErr := strconv.ParseInt(sizeArgs[1], 10, 64)
	if coresErr != nil || memoryErr != nil || cores <= 0 || memory <= 0 {
		refuse(usage)
	}
	builder := e2b.TemplateBuilder{
		APIKey:           os.Getenv("E2B_API_KEY"),
		RegistryUsername: os.Getenv("E2B_REGISTRY_USERNAME"),
		RegistryPassword: os.Getenv("E2B_REGISTRY_PASSWORD"),
		Out:              os.Stdout,
	}
	var err error
	if prune {
		var keep []e2b.TemplateOf
		for _, image := range images {
			keep = append(keep, e2b.TemplateOf{Image: image, Cores: cores, MemoryMiB: memory})
		}
		err = builder.Prune(keep)
	} else {
		err = builder.Build(images[0], cores, memory)
	}
	if err != nil {
		refuse("e2b-template: " + err.Error())
	}
}

func refuse(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
