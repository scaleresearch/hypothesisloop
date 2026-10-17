package e2b

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

const bytesPerMB = 1 << 20

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

// templateName is the name of the template for an image's digest at a size, a function of the two
// and nothing else, so there is no table of images to keep. The digest is hashed because a template
// name holds no colon.
func templateName(digest string, s size) string {
	sum := sha256.Sum256([]byte(digest))
	return fmt.Sprintf("hl-%s-%dc-%dm", hex.EncodeToString(sum[:10]), s.CPUs, s.MemoryMB)
}
