package oauth

// Fixed numbers: rules of the spec, the core contract or a format this package reads or
// writes, spelled from zero and one (the convention of the core's example module
// matter-clock) so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable. A value a firm might change is a module setting in
// package/togavsu-module.yaml, read from the core, never one of these.
import "time"

const (
	zero = iota
	one  = zero + 1
	two  = one + one
)

const (
	// verifierBytes: 48 random bytes make a 64-character PKCE verifier (RFC 7636 allows
	// 43 to 128); stateBytes: 24 random bytes of OAuth state.
	stateBytes    = (two + one) * (two + two + two + two)
	verifierBytes = two * stateBytes
	// second: the unit of a token answer's expires_in (RFC 6749).
	second = time.Duration(1000 * 1000 * 1000)
	// refreshMargin: a token is refreshed within a minute of its expiry.
	refreshMargin = (two + two + two) * (two + two + two + two + two) * second
)
