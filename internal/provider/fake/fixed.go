package fake

// Fixed numbers: rules of the spec, the core contract or a format this package reads or
// writes, spelled from zero and one (the convention of the core's example module
// matter-clock) so the core's settings source check (modulekit.CheckSource, AC-030) can
// tell a fixed rule from a tunable. A value a firm might change is a module setting in
// package/togavsu-module.yaml, read from the core, never one of these.
const (
	zero = iota
	one  = zero + 1
	two  = one + one
)

// decimal: the base UIDVALIDITY and UID are written in inside a message ref.
const decimal = (two + two + one) * two

const (
	// uidValidityStep: how far a rebuilt folder's UIDVALIDITY jumps.
	uidValidityStep = decimal * decimal
	// sentIDChars: hex characters of the raw message's SHA-256 in a sent message's id.
	sentIDChars = two * two * two * two
)
