package provider

import (
	"context"
	"time"
)

// CallTimeout bounds every call a Bounded provider makes to the vendor.
const CallTimeout = time.Minute

// Bounded wraps p so that an API which stops answering fails the pass instead of stalling it.
func Bounded(p Provider) Provider { return bounded{p} }

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
