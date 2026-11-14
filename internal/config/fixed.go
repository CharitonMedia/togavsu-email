package config

import "time"

// Fixed numbers: the units the settings are written in, spelled from zero and one (the
// convention of the core's example module matter-clock) so the core's settings source
// check (modulekit.CheckSource, AC-030) can tell a fixed rule from a tunable. The values
// themselves are module settings, read from the core.
const (
	zero  = iota
	one   = zero + 1
	six   = one + one + one + one + one + one
	ten   = six + one + one + one + one
	sixty = six * ten

	second = time.Duration(1000 * 1000 * 1000)
	minute = sixty * second
	hour   = sixty * minute
	day    = (ten + ten + one + one + one + one) * hour
)
