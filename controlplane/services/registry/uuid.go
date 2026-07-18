package registry

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// newUUIDv7 generates a UUIDv7-formatted string: 48-bit unix-ms timestamp,
// version nibble 7, 12 random bits, variant bits, then 62 random bits.
func newUUIDv7() (string, error) {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}

	ms := uint64(time.Now().UnixMilli())

	// Build 16 raw bytes.
	var b [16]byte
	// bytes 0-5: 48-bit timestamp
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// bytes 6-7: version (7) | 12 random bits
	rand12 := binary.BigEndian.Uint16(rnd[0:2]) & 0x0fff
	binary.BigEndian.PutUint16(b[6:8], 0x7000|rand12)
	// bytes 8-15: variant (10xx) | 62 random bits
	copy(b[8:], rnd[2:])
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16],
	), nil
}
