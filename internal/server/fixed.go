package server

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
	five    = two + two + one
	ten     = five + five
	hundred = ten * ten
	sixty   = (five + one) * ten

	second = time.Duration(1000 * 1000 * 1000)
	minute = sixty * second
	hour   = sixty * minute
)

const (
	// SearchLimit: a search returns at most 25 results (spec, search).
	SearchLimit = five * five
	// Skew: a signed delivery's timestamp may be at most five minutes from the module's
	// clock, as matter-clock checks events.
	Skew = five * minute
	// oauthStateTTL: an OAuth state row lives 10 minutes (spec, oauth_states).
	oauthStateTTL = ten * minute
	// imapsPort, submissionPort: the IANA ports for IMAP over TLS (RFC 8314) and mail
	// submission (RFC 6409), an IMAP account's defaults when none is given.
	imapsPort      = hundred*ten - ten + two + one
	submissionPort = five*hundred + sixty + ten + ten + five + two
	// uuidG1 to uuidG4 end the groups of a UUID's 8-4-4-4-12 hex text; uuidEnd ends it.
	uuidG1  = two * two * two
	uuidG2  = uuidG1 + two + two
	uuidG3  = uuidG2 + two + two
	uuidG4  = uuidG3 + two + two
	uuidEnd = uuidG4 + ten + two
	// randomKeyBytes: random bytes in an idempotency or correlation key.
	randomKeyBytes = ten + two
	// hoursPerDay, hoursShownUpTo: an age shows in hours under two days, then in days.
	hoursPerDay    = two*ten + two + two
	hoursShownUpTo = two * hoursPerDay
)

// Row counts the pages show. listMax bounds a full list (the queue, a matter's mail,
// the search scan); the rest are what each page or widget shows.
const (
	listMax           = five * hundred
	inboxRows         = hundred
	inboxWidgetRows   = ten
	unfiledWidgetRows = two * two * two
	matterRecentRows  = five
	matterPickRows    = two * ten
)
