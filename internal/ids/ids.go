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
	for i := range timeBytes {
		b[i] = byte(ms >> (8 * (timeBytes - one - i)))
	}
	if _, err := rand.Read(b[timeBytes:]); err != nil {
		panic(err)
	}
	b[versionByte] = b[versionByte]&0x0f | 0x70
	b[variantByte] = b[variantByte]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:group1End] + "-" + h[group1End:group2End] + "-" + h[group2End:group3End] + "-" + h[group3End:group4End] + "-" + h[group4End:]
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
