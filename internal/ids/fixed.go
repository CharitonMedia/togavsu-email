package ids

// The UUIDv7 layout (RFC 9562), spelled from zero and one (the convention of the core's
// example module matter-clock) so the core's settings source check
// (modulekit.CheckSource, AC-030) can tell a fixed rule from a tunable.
const (
	zero = iota
	one  = zero + 1
	two  = one + one
	four = two + two
	// timeBytes: the 48-bit Unix millisecond timestamp fills bytes 0 to 5.
	timeBytes = four + two
	// versionByte holds the version nibble, variantByte the variant bits.
	versionByte = timeBytes
	variantByte = four + four
	// The hex text's group boundaries: 8-4-4-4-12 characters.
	group1End = four + four
	group2End = group1End + four
	group3End = group2End + four
	group4End = group3End + four
)
