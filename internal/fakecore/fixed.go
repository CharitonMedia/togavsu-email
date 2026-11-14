package fakecore

import "time"

// Fixed numbers of the core contract this fake stands in for, spelled from zero and one
// (the convention of the core's example module matter-clock) so the core's settings
// source check (modulekit.CheckSource, AC-030) can tell a fixed rule from a tunable.
const (
	zero = iota
	one  = zero + 1
	two  = one + one
	five = two + two + one
	ten  = five + five
)

const (
	// moduleTokenBytes, tokenBytes: random bytes in the fake's module token and in
	// context, device and id tokens.
	moduleTokenBytes = two * two * two
	tokenBytes       = ten + two
	// maxSecrets: CORE-06 keeps at most 512 secrets per module.
	maxSecrets = one << (ten - one)
	// maxMatchEntries: matchMail takes at most 50 addresses, numbers or names (CORE-26).
	maxMatchEntries = five * ten
	// Page sizes the core answers with: lists, contact search and file lists.
	pageLimit        = five * ten
	contactPageLimit = two * ten
	filePageLimit    = ten * ten
)

// Lifetimes the core gives its tokens: a filing grant 15 minutes (CORE-27), an upload
// one hour, an add-in context token 10 minutes (CORE-28).
const (
	second         = time.Duration(one) * 1000 * 1000 * 1000
	minute         = (five + five) * (five + one) * second
	filingGrantTTL = (ten + five) * minute
	uploadTTL      = (ten + ten + ten) * two * minute
	deviceTokenTTL = ten * minute
)
