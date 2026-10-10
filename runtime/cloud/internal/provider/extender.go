package provider

import (
	"context"
	"time"
)

// Extender is the capability of moving the end of an instance rented with a Spec.Lifetime while
// its command runs: what rents it for a platform experiment whose end moved keeps it running to
// the new end, or ends it at an earlier one, in place of replacing it.
//
// The instance still ends by itself at the end last set, whether or not its caller or the
// vendor's API can reach it then. An end that has passed ends the command soon after it is set,
// as the lifetime's own end does.
type Extender interface {
	// Extend sets the end of the running instance id to until. It returns once the instance
	// holds the new end. One that was not rented with a lifetime, or runs a command that cannot
	// have its end moved, is an error that says so, and keeps the end it has.
	Extend(ctx context.Context, id string, until time.Time) error
}

// CapabilityExtend is the name Capabilities gives Extender.
const CapabilityExtend = "extend"
