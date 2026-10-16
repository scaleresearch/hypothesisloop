package providertest

import (
	"context"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
)

// callSpacing is the least time between two calls the suite makes. Its checks run side by side,
// and to the vendor they are one account calling three times as often.
const callSpacing = 2 * time.Second

// rentSpacing is the least time between two rentals: a vendor's offer search is the call it
// meters hardest.
const rentSpacing = 15 * time.Second

// paced lets one call through at a time. That also keeps two checks from renting in the same
// instant, when both would be offered the same last cheap machine.
type paced struct {
	provider.Provider
	mu       sync.Mutex
	last     time.Time
	lastRent time.Time
}

func (p *paced) turn() func() {
	p.mu.Lock()
	time.Sleep(time.Until(p.last.Add(callSpacing)))
	return func() {
		p.last = time.Now()
		p.mu.Unlock()
	}
}

func (p *paced) Create(ctx context.Context, spec provider.Spec) (string, error) {
	defer p.turn()()
	time.Sleep(time.Until(p.lastRent.Add(rentSpacing)))
	defer func() { p.lastRent = time.Now() }()
	return p.Provider.Create(ctx, spec)
}

func (p *paced) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	defer p.turn()()
	return p.Provider.List(ctx, namePrefix)
}

func (p *paced) Get(ctx context.Context, id string) (provider.Instance, bool, error) {
	defer p.turn()()
	return p.Provider.Get(ctx, id)
}

func (p *paced) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	defer p.turn()()
	return p.Provider.Logs(ctx, id, tail)
}

func (p *paced) Stop(ctx context.Context, id string) error {
	defer p.turn()()
	return p.Provider.Stop(ctx, id)
}

func (p *paced) Destroy(ctx context.Context, id string) error {
	defer p.turn()()
	return p.Provider.Destroy(ctx, id)
}
