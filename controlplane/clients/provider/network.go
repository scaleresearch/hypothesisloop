package provider

import "context"

// Networker is the capability of renting instances that reach one another over a private network
// of the vendor's, in one place: a physical cluster, a datacenter or a region. A network is
// known by its name alone, as an instance is, so a caller that keeps nothing between passes finds
// its own under a prefix.
//
// An instance joins a network when it is created with the network's name in Spec.Network. Create
// of such a spec returns only once the address the others reach the instance at is known, and
// fails when it cannot be learned: Get and List of the id it returns show it in
// Instance.PrivateAddress from then on.
type Networker interface {
	// Networked is how many instances of spec one private network can hold now, counted up to
	// atMost, each on a host of its own when spread. It follows Market.Rentable: only the sizes,
	// the accelerator and AvoidHosts of spec are read, it rents nothing, and an adapter whose
	// vendor meters its search answers from a read it keeps for a short while.
	Networked(ctx context.Context, spec Spec, atMost int, spread bool) (Supply, error)
	// CreateNetwork makes the network name in one place where every one of specs can be rented
	// now, each on a host of its own when spread, and ErrNoOffer when no place can. name meets
	// ValidateNetworkName. Creating a network whose name exists is no error.
	CreateNetwork(ctx context.Context, name string, specs []Spec, spread bool) error
	// Networks returns the name of every network whose name starts with prefix.
	Networks(ctx context.Context, prefix string) ([]string, error)
	// DeleteNetwork ends the network name. A caller deletes one only once no instance is on it.
	// Deleting a network that is gone is not an error.
	DeleteNetwork(ctx context.Context, name string) error
	// Interface is the network interface the private network is on inside an instance.
	Interface() string
}

// CapabilityNetwork is the name Capabilities gives Networker.
const CapabilityNetwork = "network"
