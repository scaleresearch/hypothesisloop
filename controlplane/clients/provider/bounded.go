package provider

import (
	"context"
	"time"
)

// CallTimeout bounds every call a Bounded provider makes to the vendor.
const CallTimeout = time.Minute

// Bounded is a Provider whose every call to the vendor is bounded by CallTimeout, so that an API
// which stops answering fails the pass instead of stalling it, and whose error of a call given a
// spec never quotes that spec's secrets. Each capability is the wrapped provider's, bounded the
// same way, and nil when that provider lacks it.
type Bounded struct {
	Provider
	Pauser    Pauser
	Market    Market
	Warmer    Warmer
	Networker Networker
}

// Bound wraps p in a Bounded.
func Bound(p Provider) Bounded {
	b := Bounded{Provider: bounded{p}}
	if pauser, ok := p.(Pauser); ok {
		b.Pauser = boundedPauser{pauser}
	}
	if market, ok := p.(Market); ok {
		b.Market = boundedMarket{market}
	}
	if warmer, ok := p.(Warmer); ok {
		b.Warmer = boundedWarmer{warmer}
	}
	if networker, ok := p.(Networker); ok {
		b.Networker = boundedNetworker{networker}
	}
	return b
}

type boundedNetworker struct{ networker Networker }

func (b boundedNetworker) Networked(ctx context.Context, spec Spec, atMost int, spread bool) (Supply, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	supply, err := b.networker.Networked(ctx, spec, atMost, spread)
	return supply, withoutSecrets(err, spec)
}

func (b boundedNetworker) CreateNetwork(ctx context.Context, name string, specs []Spec, spread bool) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	err := b.networker.CreateNetwork(ctx, name, specs, spread)
	for _, spec := range specs {
		err = withoutSecrets(err, spec)
	}
	return err
}

func (b boundedNetworker) Networks(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.networker.Networks(ctx, prefix)
}

func (b boundedNetworker) DeleteNetwork(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.networker.DeleteNetwork(ctx, name)
}

func (b boundedNetworker) Interface() string { return b.networker.Interface() }

type boundedWarmer struct{ warmer Warmer }

func (b boundedWarmer) Warm(ctx context.Context, spec Spec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	id, err := b.warmer.Warm(ctx, spec)
	return id, withoutSecrets(err, spec)
}

func (b boundedWarmer) StartWarm(ctx context.Context, id string, spec Spec) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return withoutSecrets(b.warmer.StartWarm(ctx, id, spec), spec)
}

type boundedMarket struct{ market Market }

func (b boundedMarket) Rentable(ctx context.Context, spec Spec, atMost int) (Supply, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	supply, err := b.market.Rentable(ctx, spec, atMost)
	return supply, withoutSecrets(err, spec)
}

type boundedPauser struct{ pauser Pauser }

func (b boundedPauser) Pause(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.pauser.Pause(ctx, id)
}

func (b boundedPauser) Resume(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.pauser.Resume(ctx, id)
}

type bounded struct{ Provider }

func (b bounded) Create(ctx context.Context, spec Spec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	id, err := b.Provider.Create(ctx, spec)
	return id, withoutSecrets(err, spec)
}

func (b bounded) List(ctx context.Context, namePrefix string) ([]Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.Provider.List(ctx, namePrefix)
}

func (b bounded) Get(ctx context.Context, id string) (Instance, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.Provider.Get(ctx, id)
}

func (b bounded) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.Provider.Logs(ctx, id, tail)
}

func (b bounded) Stop(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.Provider.Stop(ctx, id)
}

func (b bounded) Destroy(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.Provider.Destroy(ctx, id)
}
