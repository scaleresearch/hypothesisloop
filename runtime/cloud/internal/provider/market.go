package provider

import "context"

// Market is the capability of saying how much there is to rent before renting it: a vendor
// whose machines are other people's offers, which come and go and of which only some pass the
// adapter's filters. A vendor that starts a machine whenever asked has no market to count.
type Market interface {
	// Rentable is how many instances of spec could be rented right now, counted up to atMost:
	// the offers Create would choose among, under every filter it rents within, without the
	// ones this process holds. Only the sizes and the accelerator of spec are read. It rents
	// nothing, and an offer it counted may be gone by the time Create asks for it.
	Rentable(ctx context.Context, spec Spec, atMost int) (int, error)
}

// CapabilityMarket is the name Capabilities gives Market.
const CapabilityMarket = "market"
