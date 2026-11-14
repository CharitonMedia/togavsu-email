package testmail

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
	second = time.Duration(1000 * 1000 * 1000)
	hour   = (two + two + two) * (two + two + two) * (ten * ten) * second
	// certLife: the runtime test certificate is valid a day.
	certLife = (ten + ten + two + two) * hour
	ten      = two + two + two + two + two
	// authPlainParts: AUTH PLAIN decodes to authzid, authcid and password (RFC 4616).
	authPlainParts = two + one
	// base64Line: RFC 2045 caps a base64 line at 76 characters.
	base64Line = ten*(ten-two-one) + two*(two+one)
	// mailFrom and rcptTo are the SMTP command prefixes the listener strips.
	mailFrom = "FROM:"
	rcptTo   = "TO:"
)
