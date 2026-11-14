package syncer

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
	five  = two + two + one
	ten   = five + five
	sixty = (five + one) * ten

	second = time.Duration(1000 * 1000 * 1000)
	minute = sixty * second
	hour   = sixty * minute
)

const (
	// RematchEvery is the sweep of the unfiled queue (spec: hourly).
	RematchEvery = hour
	// UnfiledNoticeGap is the least time between two unfiled_mail notices to one person
	// (spec: at most once every 15 minutes).
	UnfiledNoticeGap = (ten + five) * minute
	// BatchLimit is how many new messages one folder reads per run.
	BatchLimit = two * ten * ten
	// tick: the loop wakes once a minute; each account syncs when its poll_minutes
	// setting has passed.
	tick = minute
)
