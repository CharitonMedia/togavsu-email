package render

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

// The filed copy's name keeps at most eight subject words and 60 slug characters.
const (
	fileNameWords   = two * two * two
	fileNameSlugMax = (two + two + two) * (two + two + two + two + two)
)
