package agentlaunch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// The warm pool is instances rented ahead of the agents that will run in them: placed, their
// image on their host, running nothing and billed for their disk only. A missing agent is
// started in one of them where one was warmed for its image, which takes seconds where a create
// takes the image's pull, and the pool is topped up after.
//
// A pool instance is named poolPrefix and a digest of the image and this launcher's launch
// inputs, so it lives under the launcher's own prefix, which one List already covers, and is
// told apart from an agent's by a segment no control-plane name starts with (read refuses one
// that does). Like everything else it is kept nowhere but in the provider's list: each pass
// destroys what does not belong, and a restarted process takes over the pool it finds.
//
// Which image the pool holds follows the answers: the image of an agent asked for that the pool
// already holds, or else the one most agents are asked for under. An answer that asks for no
// agent leaves the pool as it is.

// poolSegment follows the launcher's prefix in a pool instance's name.
const poolSegment = "pool-"

// warmLimit is how long a pool instance may take to become warm before it is given up.
const warmLimit = 10 * time.Minute

// drainLimit bounds destroying the pool when the process ends.
const drainLimit = time.Minute

// poolName is the name of a pool instance that can start an agent of image.
func (l *Launcher) poolName(image string) string {
	digest := sha256.Sum256([]byte(image + "\x00" + l.launchDigest))
	return l.poolPrefix + hex.EncodeToString(digest[:])[:16]
}

// readyPool is the warm instances of pool by name, oldest first.
func (l *Launcher) readyPool(pool []provider.Instance) map[string][]provider.Instance {
	ready := map[string][]provider.Instance{}
	if l.cfg.WarmPool == 0 || l.warmer == nil {
		return ready
	}
	for _, i := range sortedByID(pool) {
		if i.State == provider.StateWarm {
			ready[i.Name] = append(ready[i.Name], i)
		}
	}
	return ready
}

// startWarm starts spec on a warm instance of its image and reports whether one runs it. A pool
// instance that fails to start is destroyed, and the caller creates one instead.
func (p *pass) startWarm(ctx context.Context, spec provider.Spec) (string, bool) {
	l := p.l
	name := l.poolName(spec.Image)
	if len(p.ready[name]) == 0 {
		return "", false
	}
	i := p.ready[name][0]
	p.ready[name] = p.ready[name][1:]
	p.started[i.ID] = true
	if err := l.warmer.StartWarm(ctx, i.ID, spec); err != nil {
		err = withoutEnvironment(err, spec.Env)
		l.cfg.Log("start %s on warm instance %s: %v; it is destroyed and the agent created", spec.Name, i.ID, err)
		p.destroy(ctx, i, "its start failed")
		return "", false
	}
	return i.ID, true
}

// keepPool brings the pool to WarmPool instances of the image the answer calls for. What else
// is in it goes: another image or launch, more than WarmPool, one that runs, has run or did
// not become warm within warmLimit. A pool instance is never left running.
func (p *pass) keepPool(ctx context.Context, pool []provider.Instance, want *desired) {
	l := p.l
	image := l.poolImage(pool, want)
	kept := 0
	for _, i := range sortedByID(pool) {
		if p.started[i.ID] {
			continue
		}
		why := ""
		switch {
		case l.cfg.WarmPool == 0 || l.warmer == nil:
			why = "warm pool is off"
		case i.State != provider.StateWarm && i.State != provider.StateStarting:
			why = fmt.Sprintf("a pool instance that is %s", i.State)
		case i.State == provider.StateStarting && time.Since(i.StartedAt) > warmLimit:
			why = fmt.Sprintf("a pool instance not warm after %s", warmLimit)
		case image != "" && i.Name != l.poolName(image):
			why = "a pool instance for another image or launch"
		case kept >= l.cfg.WarmPool:
			why = "more pool instances than warm_pool_size"
		}
		if why != "" {
			p.destroy(ctx, i, why)
			continue
		}
		kept++
	}
	if image == "" || want.refusal != "" || l.warmer == nil {
		return
	}
	spec := provider.Spec{
		Name: l.poolName(image), Image: image,
		CPUMillis: l.cfg.Size.CPUMillis, MemoryBytes: l.cfg.Size.MemoryBytes, DiskBytes: l.cfg.Size.DiskBytes,
	}
	for ; kept < l.cfg.WarmPool; kept++ {
		if !p.rent(ctx) {
			return
		}
		id, err := l.warmer.Warm(ctx, spec)
		if err != nil {
			l.cfg.Log("warm a pool instance of %s: %v", image, err)
			p.failures = append(p.failures, fmt.Errorf("warm a pool instance: %w", err))
			return
		}
		l.cfg.Log("warming pool instance %s of %s", id, image)
	}
}

// poolImage is the image the pool should hold after this answer, "" for no change: the image
// of an agent asked for that a live pool instance holds, or else the one most agents are asked
// for under, the first in order of a tie.
func (l *Launcher) poolImage(pool []provider.Instance, want *desired) string {
	counts := map[string]int{}
	for _, i := range want.instances {
		counts[i.image] = counts[i.image] + 1
	}
	if len(counts) == 0 {
		return ""
	}
	images := sortedKeys(counts)
	for _, i := range pool {
		for _, image := range images {
			if i.Name == l.poolName(image) && (i.State == provider.StateWarm || i.State == provider.StateStarting) {
				return image
			}
		}
	}
	sort.SliceStable(images, func(a, b int) bool { return counts[images[a]] > counts[images[b]] })
	return images[0]
}

// drainPool destroys every pool instance, for a process that ends. The agents' own instances
// are left to the next one.
func (l *Launcher) drainPool(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, drainLimit)
	defer cancel()
	rented, err := l.provider.List(ctx, l.poolPrefix)
	if err != nil {
		l.cfg.Log("the warm pool could not be listed and may still be rented: %v", err)
		return
	}
	for _, i := range rented {
		if !strings.HasPrefix(i.Name, l.poolPrefix) {
			continue
		}
		if err := l.provider.Destroy(ctx, i.ID); err != nil {
			l.cfg.Log("destroy pool instance %s: %v; it may still be rented", i.ID, err)
			continue
		}
		l.cfg.Log("destroyed pool instance %s (the launcher stops)", i.ID)
	}
}

func sortedByID(instances []provider.Instance) []provider.Instance {
	sorted := append([]provider.Instance(nil), instances...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].ID < sorted[b].ID })
	return sorted
}
