package provider

import "context"

// Market is the capability of saying how much there is to rent before renting it: a vendor
// whose machines are other people's offers, which come and go and of which only some pass the
// adapter's filters. A vendor that starts a machine whenever asked has no market to count.
type Market interface {
	// Rentable is how many instances of spec could be rented right now, counted up to atMost:
	// the offers Create would choose among, under every filter it rents within, without the
	// ones this process holds. Only the sizes, the accelerator and AvoidHosts of spec are read.
	// It rents nothing, and an offer it counted may be gone by the time Create asks for it.
	// A caller asks on every pass: an adapter whose vendor meters its search answers from a
	// read it keeps for a short while.
	Rentable(ctx context.Context, spec Spec, atMost int) (Supply, error)
}

// Supply is one answer of a Market.
type Supply struct {
	Count int
	// Why says, when Count is 0, what keeps the vendor's machines out, in the adapter's terms:
	// that it has none of spec's size, or which of its filters leave out the ones it has. It
	// names no figures, so it stays the same while the cause does, and it is shown to whoever
	// waits for a machine. Empty when Count is above 0. The adapter logs the figures behind it.
	Why string
}

// CapabilityMarket is the name Capabilities gives Market.
const CapabilityMarket = "market"
