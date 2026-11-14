// Package ids makes the module's row ids: UUIDv7 text, ordered by creation time.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// New returns a UUIDv7 for t.
func New(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Now returns a UUIDv7 for the current time.
func Now() string { return New(time.Now()) }

// Secret returns n random bytes as hex, for OAuth state and PKCE verifiers.
func Secret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
