// Package match holds the section 8.3 rules: which matter, if any, one message files to.
// The rules are pure functions over the message, the account and the core's matchMail
// answer (CC-3), so they test with injected answers; the one outside call, the model
// pick for court notices, comes in as a function.
package match

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
)

// Outcomes.
const (
	File  = "file"
	Queue = "queue"
	Inbox = "inbox" // personal mail with no contact: stays in the owner's inbox
)

// How values, as filings.how stores them.
const (
	HowToken     = "auto_token"
	HowThread    = "auto_thread"
	HowAddress   = "auto_address"
	HowCourt     = "auto_court"
	HowCourtName = "auto_court_name"
	HowCourtAI   = "auto_court_ai"
)

// Reason codes for a message that files nowhere.
const (
	NoExternalAddress   = "no_external_address"
	NoContact           = "no_contact"
	MultipleContacts    = "multiple_contacts"
	NoOpenMatter        = "no_open_matter"
	MultipleOpenMatters = "multiple_open_matters"
	TokenMultiple       = "token_multiple_matters"
	TokenNoMatter       = "token_no_matter"
	PartyMismatch       = "case_number_matched_but_party_name_mismatch"
	MultipleCaseMatches = "multiple_case_matches"
	NoCaseNumber        = "no_case_number"
	NoCourtMatch        = "no_court_match"
	MultipleNameMatches = "multiple_name_matches"
	AINone              = "ai_none"
	AINotCandidate      = "ai_pick_not_candidate"
	AIFailed            = "ai_pick_failed"
)

// DefaultPatterns is the case_number_patterns default: trial first, appellate second.
const DefaultPatterns = `\b(\d{2}[A-Z]{2,4}-[A-Z]{2,3}\d{4,8}(?:-\d{1,3})?)\b;\b((?:SC|ED|WD|SD)\d{4,6})\b`

// Input is what the rules know about one message.
type Input struct {
	Role           string // personal, filing, court
	PlusAddressing bool
	AccountAddress string
	FirmDomains    []string
	From, To, Cc   []string
	DeliveredTo    []string
	Subject, Text  string
	// ThreadMatters are the matters of filed messages this one replies to or references.
	ThreadMatters []string
	AIDeclined    bool
}

// Config is the module settings the rules read.
type Config struct {
	Patterns []*regexp.Regexp
	AIPick   bool // court_ai_pick and the ai:use grant
}

// Candidate is one saved suggestion.
type Candidate struct {
	MatterID string
	Via      string
	Rank     int
}

// Decision is the rules' answer.
type Decision struct {
	Outcome    string
	MatterID   string
	How        string
	Reason     string
	Candidates []Candidate
	AIAsked    bool
	AIDeclined bool // the model said none this time
	// Contacts maps each external address to its contact id, when exactly one holds it.
	Contacts map[string]string
}

// Picker asks the model which candidate a court notice belongs to. It answers a
// candidate's matter id, any other id, or "" for none.
type Picker func(ctx context.Context, input map[string]any) (string, error)

// ParsePatterns reads the semicolon-separated case_number_patterns setting. Invalid
// patterns are skipped; an empty setting uses the default.
func ParsePatterns(s string) []*regexp.Regexp {
	if strings.TrimSpace(s) == "" {
		s = DefaultPatterns
	}
	var out []*regexp.Regexp
	for _, p := range strings.Split(s, ";") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// Direction is court for a court account, outbound when every From address is a firm
// address, inbound otherwise.
func Direction(in Input) string {
	if in.Role == "court" {
		return "court"
	}
	if len(in.From) == 0 {
		return "inbound"
	}
	for _, a := range in.From {
		if !IsFirm(a, in.FirmDomains) {
			return "inbound"
		}
	}
	return "outbound"
}

// IsFirm reports whether addr is at one of the firm's domains.
func IsFirm(addr string, domains []string) bool {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return false
	}
	d := strings.ToLower(addr[at+1:])
	for _, f := range domains {
		if strings.EqualFold(strings.TrimSpace(f), d) {
			return true
		}
	}
	return false
}

// External lists the message's non-firm addresses, From first, without repeats.
func External(in Input) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{in.From, in.To, in.Cc} {
		for _, a := range l {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" || seen[a] || IsFirm(a, in.FirmDomains) {
				continue
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// PlusToken finds the token in filing+TOKEN@ on the account's own address, looking at
// Delivered-To first and then To and Cc.
func PlusToken(in Input) string {
	if in.Role != "filing" || !in.PlusAddressing {
		return ""
	}
	at := strings.LastIndexByte(in.AccountAddress, '@')
	if at < 0 {
		return ""
	}
	local, domain := strings.ToLower(in.AccountAddress[:at]), strings.ToLower(in.AccountAddress[at+1:])
	for _, l := range [][]string{in.DeliveredTo, in.To, in.Cc} {
		for _, a := range l {
			a = strings.ToLower(a)
			i := strings.LastIndexByte(a, '@')
			if i < 0 || a[i+1:] != domain || !strings.HasPrefix(a[:i], local+"+") {
				continue
			}
			if t := a[len(local)+1 : i]; t != "" {
				return t
			}
		}
	}
	return ""
}

// CaseNumbers finds the court case numbers in a notice: the first pattern that finds
// any wins, so the trial pattern goes first and the appellate one is a fallback. The full
// number is kept, never with its count suffix stripped.
func CaseNumbers(text string, patterns []*regexp.Regexp) []string {
	for _, re := range patterns {
		var out []string
		seen := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			n := m[0]
			if len(m) > 1 && m[1] != "" {
				n = m[1]
			}
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

var stop = map[string]bool{"the": true, "and": true, "for": true, "of": true, "in": true, "on": true, "to": true, "by": true, "re": true, "fw": true,
	"fwd": true, "court": true, "county": true, "circuit": true, "case": true, "notice": true, "filed": true, "filing": true, "behalf": true,
	"state": true, "plaintiff": true, "defendant": true, "petitioner": true, "respondent": true, "et": true, "al": true, "vs": true, "v": true,
	"electronic": true, "service": true, "missouri": true, "district": true, "appeals": true, "order": true, "motion": true, "judge": true}

// NameTokens lists capitalized words from the notice's subject and opening text, the
// surnames the core checks against clients (at most 50).
func NameTokens(subject, text string) []string {
	if len(text) > 4000 {
		text = text[:4000]
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(subject+"\n"+text, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		w = strings.Trim(w, "'")
		r := []rune(w)
		if len(r) < 2 || !unicode.IsUpper(r[0]) {
			continue
		}
		l := strings.ToLower(w)
		if stop[l] || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
		if len(out) == 50 {
			break
		}
	}
	return out
}

// Request builds the matchMail body for a message.
func Request(in Input, cfg Config) coreapi.MatchRequest {
	r := coreapi.MatchRequest{Addresses: External(in), KnownMatterIDs: in.ThreadMatters}
	if in.Role == "court" {
		r.Addresses = nil
		r.CaseNumbers = CaseNumbers(in.Subject+"\n"+in.Text, cfg.Patterns)
		r.NameTokens = NameTokens(in.Subject, in.Text)
	}
	if t := PlusToken(in); t != "" {
		r.MatterNumbers = []string{t}
		r.CaseNumbers = append(r.CaseNumbers, strings.ToUpper(t))
		r.NameTokens = append(r.NameTokens, t)
	}
	return r
}

func live(status string) bool { return status == "open" || status == "on_hold" }

type cands struct {
	list []Candidate
	seen map[string]bool
}

func (c *cands) add(id, via string) {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if c.seen[id] {
		return
	}
	c.seen[id] = true
	c.list = append(c.list, Candidate{MatterID: id, Via: via, Rank: len(c.list)})
}

func queue(reason string, c cands) Decision {
	return Decision{Outcome: Queue, Reason: reason, Candidates: c.list}
}

func file(id, how, via string) Decision {
	return Decision{Outcome: File, MatterID: id, How: how, Candidates: []Candidate{{MatterID: id, Via: via}}}
}

// Decide applies the section 8.3 order after the duplicate check, which the caller
// makes against its own filings: plus token, thread, court, address.
func Decide(ctx context.Context, in Input, cfg Config, ans coreapi.MatchAnswer, pick Picker) (Decision, error) {
	var c cands

	// 2. Plus token: number or case number, any status; else client last name, one live matter.
	tokenReason := ""
	if t := PlusToken(in); t != "" {
		var byNumber, byName []coreapi.MatchMatter
		for _, m := range ans.Matters {
			if (m.HasVia("matter_number") && strings.EqualFold(m.Number, t)) || (m.HasVia("case_number") && strings.EqualFold(m.CaseNumber, t)) {
				byNumber = append(byNumber, m)
			} else if m.HasVia("client_name") && live(m.Status) && clientSurname(m, t) {
				byName = append(byName, m)
			}
		}
		switch {
		case len(byNumber) == 1:
			return file(byNumber[0].ID, HowToken, "token"), nil
		case len(byNumber) > 1:
			for _, m := range byNumber {
				c.add(m.ID, "token")
			}
			return queue(TokenMultiple, c), nil
		case len(byName) == 1:
			return file(byName[0].ID, HowToken, "token"), nil
		case len(byName) > 1:
			for _, m := range byName {
				c.add(m.ID, "token")
			}
			return queue(TokenMultiple, c), nil
		}
		tokenReason = TokenNoMatter
	}

	// 3. Thread: a filed message's matter the core returns as known, open or on hold.
	if len(in.ThreadMatters) > 0 {
		var hits []coreapi.MatchMatter
		for _, m := range ans.Matters {
			if m.HasVia("known") && contains(in.ThreadMatters, m.ID) && live(m.Status) {
				hits = append(hits, m)
			}
		}
		if len(hits) == 1 {
			return file(hits[0].ID, HowThread, "thread"), nil
		}
		for _, m := range hits {
			c.add(m.ID, "thread")
		}
	}

	// 4. Court notices.
	if in.Role == "court" {
		return court(ctx, in, cfg, ans, pick, c)
	}

	// 5. Address.
	d := address(in, ans, c)
	if d.Outcome == Queue && tokenReason != "" && (d.Reason == NoContact || d.Reason == NoExternalAddress) {
		d.Reason = tokenReason
	}
	return d, nil
}

func address(in Input, ans coreapi.MatchAnswer, c cands) Decision {
	ext := External(in)
	personal := in.Role == "personal"
	if len(ext) == 0 {
		if personal {
			return Decision{Outcome: Inbox, Reason: NoExternalAddress, Candidates: c.list}
		}
		return queue(NoExternalAddress, c)
	}
	contactIDs := map[string]bool{}
	byAddr := map[string]string{}
	for _, a := range ext {
		cs := ans.ContactsByAddress[a]
		for _, ct := range cs {
			contactIDs[ct.ContactID] = true
		}
		if len(cs) == 1 {
			byAddr[a] = cs[0].ContactID
		}
	}
	switch {
	case len(contactIDs) == 0:
		if personal {
			return Decision{Outcome: Inbox, Reason: NoContact, Candidates: c.list}
		}
		d := queue(NoContact, c)
		return d
	case len(contactIDs) > 1:
		d := queue(MultipleContacts, c)
		d.Contacts = byAddr
		return d
	}
	var open []coreapi.MatchMatter
	for _, m := range ans.Matters {
		if m.HasVia("address") && live(m.Status) {
			open = append(open, m)
		}
	}
	for _, m := range open {
		c.add(m.ID, "address")
	}
	var d Decision
	switch len(open) {
	case 0:
		d = queue(NoOpenMatter, c)
	case 1:
		d = file(open[0].ID, HowAddress, "address")
	default:
		d = queue(MultipleOpenMatters, c)
	}
	d.Contacts = byAddr
	return d
}

func court(ctx context.Context, in Input, cfg Config, ans coreapi.MatchAnswer, pick Picker, c cands) (Decision, error) {
	caption := in.Subject + "\n" + in.Text
	numbers := CaseNumbers(caption, cfg.Patterns)

	// Case number: exact on the field, or written in the name with a boundary that
	// excludes a following hyphen; any status; the party text must name the client.
	var byNumber []coreapi.MatchMatter
	for _, m := range ans.Matters {
		for _, n := range numbers {
			if (m.CaseNumber != "" && m.CaseNumber == n) || InName(m.Name, n) {
				byNumber = append(byNumber, m)
				break
			}
		}
	}
	if len(byNumber) > 0 {
		var ok []coreapi.MatchMatter
		for _, m := range byNumber {
			c.add(m.ID, viaNumber(m, numbers))
			if corroborated(m, caption) {
				ok = append(ok, m)
			}
		}
		switch len(ok) {
		case 1:
			d := file(ok[0].ID, HowCourt, viaNumber(ok[0], numbers))
			return d, nil
		case 0:
			return queue(PartyMismatch, c), nil
		default:
			return queue(MultipleCaseMatches, c), nil
		}
	}

	// Client first and last name in the caption, exactly one matter.
	var byName, surname []coreapi.MatchMatter
	for _, m := range ans.Matters {
		if !m.HasVia("client_name") {
			continue
		}
		surname = append(surname, m)
		if fullName(m, caption) {
			byName = append(byName, m)
		}
	}
	if len(byName) == 1 {
		return file(byName[0].ID, HowCourtName, "client_name"), nil
	}
	for _, m := range surname {
		c.add(m.ID, "client_name")
	}

	// The model picks among surname candidates, once per message.
	if len(surname) > 0 && cfg.AIPick && !in.AIDeclined && pick != nil {
		input := map[string]any{"subject": in.Subject, "text": cut(in.Text, 8000), "case_numbers": numbers, "candidates": candidateInput(surname)}
		id, err := pick(ctx, input)
		if err != nil {
			d := queue(AIFailed, c)
			return d, nil
		}
		if id == "" {
			d := queue(AINone, c)
			d.AIAsked, d.AIDeclined = true, true
			return d, nil
		}
		for _, m := range surname {
			if m.ID == id {
				d := file(id, HowCourtAI, "ai")
				d.AIAsked = true
				return d, nil
			}
		}
		d := queue(AINotCandidate, c)
		d.AIAsked = true
		return d, nil
	}
	switch {
	case len(byName) > 1:
		return queue(MultipleNameMatches, c), nil
	case len(numbers) == 0 && len(surname) == 0:
		return queue(NoCaseNumber, c), nil
	case in.AIDeclined && len(surname) > 0:
		return queue(AINone, c), nil
	}
	return queue(NoCourtMatch, c), nil
}

func viaNumber(m coreapi.MatchMatter, numbers []string) string {
	for _, n := range numbers {
		if m.CaseNumber == n {
			return "case_number"
		}
	}
	return "case_number_in_name"
}

func candidateInput(ms []coreapi.MatchMatter) []map[string]any {
	sort.Slice(ms, func(i, j int) bool { return ms[i].ID < ms[j].ID })
	var out []map[string]any
	for _, m := range ms {
		var cl []string
		for _, c := range m.Clients {
			cl = append(cl, strings.TrimSpace(c.GivenName+" "+c.FamilyName))
		}
		out = append(out, map[string]any{"matter_id": m.ID, "number": m.Number, "name": m.Name, "case_number": m.CaseNumber, "status": m.Status, "clients": cl})
	}
	return out
}

// InName reports cn written in a matter name on a boundary that excludes a following
// hyphen, so 26AB-CV00123 is not found in "Doe 26AB-CV00123-01".
func InName(name, cn string) bool {
	if cn == "" {
		return false
	}
	n, k := strings.ToUpper(name), strings.ToUpper(cn)
	for i := 0; i < len(n); {
		j := strings.Index(n[i:], k)
		if j < 0 {
			return false
		}
		s, e := i+j, i+j+len(k)
		if (s == 0 || !alnum(n[s-1])) && (e == len(n) || (!alnum(n[e]) && n[e] != '-')) {
			return true
		}
		i = s + 1
	}
	return false
}

func alnum(b byte) bool { return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }

// words lower-cases text into letter runs.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' })
}

func hasWord(text, w string) bool {
	w = strings.ToLower(w)
	for _, x := range words(text) {
		if x == w {
			return true
		}
	}
	return false
}

func clientSurname(m coreapi.MatchMatter, token string) bool {
	for _, c := range m.Clients {
		if strings.EqualFold(c.FamilyName, token) {
			return true
		}
	}
	return false
}

// corroborated: the party text contains a client's last name.
func corroborated(m coreapi.MatchMatter, caption string) bool {
	for _, c := range m.Clients {
		if c.FamilyName != "" && hasWord(caption, c.FamilyName) {
			return true
		}
	}
	return false
}

// fullName: a client's first and last name appear together in the caption, as
// "Given Family" or "Family, Given".
func fullName(m coreapi.MatchMatter, caption string) bool {
	w := words(caption)
	for _, c := range m.Clients {
		g, f := strings.ToLower(c.GivenName), strings.ToLower(c.FamilyName)
		if g == "" || f == "" {
			continue
		}
		for i := 0; i+1 < len(w); i++ {
			if (w[i] == g && w[i+1] == f) || (w[i] == f && w[i+1] == g) {
				return true
			}
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
