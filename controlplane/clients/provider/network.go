package provider

import "context"

// Networker is the capability of renting instances that reach one another over a private network
// of the vendor's, in one place: a physical cluster, a datacenter or a region. The instances in
// one place are as near each other as Within says.
//
// A place is what Place chose, and an instance is rented in it when it is created with it in
// Spec.Place. A place is no object of the caller's: Place makes nothing to end, and what an
// instance joins ends with the instance. An adapter whose vendor needs an object of its own for a
// private network keeps it in step with the instances on it. Create of a spec with a place
// returns only once the address the others reach the instance at is known, and fails when it
// cannot be learned: Get and List of the id it returns show it in Instance.PrivateAddress from
// then on.
type Networker interface {
	// Networked is how many instances of spec one place can hold now, counted up to atMost. It
	// follows Market.Rentable: only the sizes, the accelerator and AvoidHosts of spec are read, it
	// rents nothing, and an adapter whose vendor meters its search answers from a read it keeps
	// for a short while.
	Networked(ctx context.Context, spec Spec, atMost int) (Supply, error)
	// Place chooses one place where every one of specs can be rented now, and is ErrNoOffer when
	// no place can. It rents nothing, and a place it chose may be full by the time Create asks
	// for it.
	Place(ctx context.Context, specs []Spec) (string, error)
	// Interface is the network interface the private network is on inside an instance.
	Interface() string
	// Within is the nearest level every instance in one place is within, one of
	// clusterapi.NodesLevels: placement_group, zone or region.
	Within() string
}

// CapabilityNetwork is the name Capabilities gives Networker.
const CapabilityNetwork = "network"
