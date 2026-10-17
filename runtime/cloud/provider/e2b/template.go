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

// The most any plan gives a sandbox: a template holds up to maxCPUs cores and maxMemoryMB of
// memory, and the disk is the plan's, 10 GiB on the free plan and maxDiskGB on Pro. A spec beyond
// the free plan's disk on that plan fails at Create.
const (
	maxCPUs     = 8
	maxMemoryMB = 8192
	maxDiskGB   = 20
)

// CheckSize refuses a spec larger than any sandbox (provider.Sizer).
func (p *Provider) CheckSize(spec provider.Spec) error {
	asked := sizeOf(spec)
	if disk := provider.CeilDiv(spec.DiskBytes, 1<<30); asked.CPUs > maxCPUs || asked.MemoryMB > maxMemoryMB || disk > maxDiskGB {
		return fmt.Errorf("e2b: spec %q asks for %d cores, %d MiB of memory and %d GiB of disk, and a sandbox has at most %d, %d and %d",
			spec.Name, asked.CPUs, asked.MemoryMB, disk, maxCPUs, maxMemoryMB, maxDiskGB)
	}
	return nil
}

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
