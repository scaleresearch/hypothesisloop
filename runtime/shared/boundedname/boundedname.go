// Package boundedname is the one way a runtime shortens a name it builds from ids.
package boundedname

import (
	"crypto/sha256"
	"encoding/hex"
)

const digestLength = 12

// Name returns readable when it is at most limit long. A longer one keeps its start and ends in a
// dash and 12 hex digits of the SHA-256 of identity, which is everything the name stands for. A
// cut alone would name two workloads alike: an id is a UUIDv7, whose start is its timestamp, and
// group names of one job share the whole id as their start.
func Name(readable, identity string, limit int) string {
	if len(readable) <= limit {
		return readable
	}
	digest := sha256.Sum256([]byte(identity))
	suffix := "-" + hex.EncodeToString(digest[:])[:digestLength]
	return readable[:limit-len(suffix)] + suffix
}
