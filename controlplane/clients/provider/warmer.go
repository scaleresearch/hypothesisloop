package provider

import "context"

// Warmer is the capability of renting an instance before it is known what it will run: the
// vendor places it and makes its image ready, it runs nothing and is billed for its disk only,
// and later it is given a spec and started, which takes a fraction of what a Create does.
//
// A warm instance is never running. One that is listed in any state but StateStarting or
// StateWarm was started by something else, and a caller destroys it.
type Warmer interface {
	// Warm rents an instance under spec.Name that runs nothing. Of spec only Name, Image and the
	// sizes are read. It returns once List shows the instance: StateStarting while the vendor
	// makes it ready and StateWarm after. An error leaves nothing, or an instance List shows
	// under spec.Name.
	Warm(ctx context.Context, spec Spec) (id string, err error)
	// StartWarm makes the warm instance id the instance of spec and starts its command, as Create
	// would have: List shows it under spec.Name from when it returns. spec has the image and the
	// sizes the instance was warmed with. An error may leave the instance in any state under
	// either name, and the caller destroys it.
	StartWarm(ctx context.Context, id string, spec Spec) error
}

// CapabilityWarm is the name Capabilities gives Warmer.
const CapabilityWarm = "warm"
