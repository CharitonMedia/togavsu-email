package match

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

const (
	ten      = (two + two + one) * two
	thousand = ten * ten * ten
	// matchEntriesMax: matchMail takes at most 50 name tokens (spec, CORE-26).
	matchEntriesMax = (two + two + one) * ten
	// nameTextMax: surnames are read from the subject and the first 4,000 bytes of text.
	nameTextMax = (two + two) * thousand
	// aiTextMax: the court AI pick sees at most 8,000 bytes of the notice text.
	aiTextMax = two * nameTextMax
)
