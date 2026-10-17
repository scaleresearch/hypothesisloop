package e2b

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

const bytesPerMB = 1 << 20

// templateCommand builds the template an image and size need (TemplateBuilder), run from the
// repository's root with the image, the cores and the memory in MiB after it.
const templateCommand = "go run ./controlplane/cmd/e2b-template"

// size is what a template fixes for every sandbox started from it.
type size struct {
	CPUs     int64
	MemoryMB int64
}

func sizeOf(spec provider.Spec) size {
	return size{CPUs: provider.CeilDiv(spec.CPUMillis, 1000), MemoryMB: provider.CeilDiv(spec.MemoryBytes, bytesPerMB)}
}

// templateName is the name of the template for an image at a size. It is a function of the two
// and nothing else, so there is no table of images to keep: TemplateBuilder builds it under the same
// name when it builds the template, and a spec whose template was never built fails at Create
// with the command that builds it. The image reference is hashed because a template name holds
// neither slashes nor colons.
func templateName(image string, s size) string {
	digest := sha256.Sum256([]byte(image))
	return fmt.Sprintf("%s%s-%dc-%dm", templatePrefix, hex.EncodeToString(digest[:10]), s.CPUs, s.MemoryMB)
}

// templatePrefix starts the name of every template built here, which is how Prune tells them
// from a template the team made some other way.
const templatePrefix = "hl-"
