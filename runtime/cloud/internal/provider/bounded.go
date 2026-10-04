package provider

import (
	"context"
	"time"
)

// CallTimeout bounds every call a Bounded provider makes to the vendor.
const CallTimeout = time.Minute

// Bounded wraps p so that an API which stops answering fails the pass instead of stalling it.
// What it returns has the capabilities p has.
func Bounded(p Provider) Provider {
	pauser, pauses := p.(Pauser)
	market, counts := p.(Market)
	warmer, warms := p.(Warmer)
	b, bp, bm, bw := bounded{p}, boundedPauser{pauser}, boundedMarket{market}, boundedWarmer{warmer}
	switch {
	case pauses && counts && warms:
		return struct {
			bounded
			boundedPauser
			boundedMarket
			boundedWarmer
		}{b, bp, bm, bw}
	case pauses && counts:
		return struct {
			bounded
			boundedPauser
			boundedMarket
		}{b, bp, bm}
	case pauses && warms:
		return struct {
			bounded
			boundedPauser
			boundedWarmer
		}{b, bp, bw}
	case counts && warms:
		return struct {
			bounded
			boundedMarket
			boundedWarmer
		}{b, bm, bw}
	case pauses:
		return struct {
			bounded
			boundedPauser
		}{b, bp}
	case counts:
		return struct {
			bounded
			boundedMarket
		}{b, bm}
	case warms:
		return struct {
			bounded
			boundedWarmer
		}{b, bw}
	}
	return b
}

type boundedWarmer struct{ warmer Warmer }

func (b boundedWarmer) Warm(ctx context.Context, spec Spec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.warmer.Warm(ctx, spec)
}

func (b boundedWarmer) StartWarm(ctx context.Context, id string, spec Spec) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.warmer.StartWarm(ctx, id, spec)
}

type boundedMarket struct{ market Market }

func (b boundedMarket) Rentable(ctx context.Context, spec Spec, atMost int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.market.Rentable(ctx, spec, atMost)
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
	return b.Provider.Create(ctx, spec)
}

func (b bounded) List(ctx context.Context, namePrefix string) ([]Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return b.Provider.List(ctx, namePrefix)
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
