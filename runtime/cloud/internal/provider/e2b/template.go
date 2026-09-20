package e2b

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const bytesPerMB = 1 << 20

// templateScript builds the template an image and size need.
const templateScript = "infra/images/e2b-template.sh"

// size is what a template fixes for every sandbox started from it.
type size struct {
	CPUs     int64
	MemoryMB int64
}

func sizeOf(spec provider.Spec) size {
	return size{CPUs: ceilDiv(spec.CPUMillis, 1000), MemoryMB: ceilDiv(spec.MemoryBytes, bytesPerMB)}
}

// templateName is the name of the template for an image at a size. It is a function of the two
// and nothing else, so there is no table of images to keep: templateScript computes the same
// name when it builds the template, and a spec whose template was never built fails at Create
// with the command that builds it. The image reference is hashed because a template name holds
// neither slashes nor colons.
func templateName(image string, s size) string {
	digest := sha256.Sum256([]byte(image))
	return fmt.Sprintf("hl-%s-%dc-%dm", hex.EncodeToString(digest[:10]), s.CPUs, s.MemoryMB)
}

func ceilDiv(value, unit int64) int64 {
	return (value + unit - 1) / unit
}
