package email_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/testmail"
)

const courtSender = "noticing@court.example.test"

func (e *env) courtNotice(subject, text string) testmail.Msg {
	return testmail.Msg{ID: randomHex(6) + "@court.example.test", From: "Superior Court <" + courtSender + ">", To: []string{courtAddr},
		Subject: subject, Text: text, Date: time.Now().Add(-time.Hour)}
}

func (e *env) filedTo(t *testing.T, matterID, how string) filing {
	t.Helper()
	fs := e.filings()
	if len(fs) != 1 || fs[0].MatterID != matterID || (how != "" && fs[0].How != how) {
		t.Fatalf("filings %+v, want one to %s by %s; rows %+v", fs, matterID, how, e.rows())
	}
	return fs[0]
}

func (e *env) queued(t *testing.T, reason string) row {
	t.Helper()
	rs := e.only("unfiled")
	if len(rs) != 1 || (reason != "" && rs[0].Reason != reason) {
		t.Fatalf("unfiled rows %+v, want one with %s; all %+v", rs, reason, e.rows())
	}
	if n := len(e.filings()); n != 0 {
		t.Fatalf("%d filings", n)
	}
	return rs[0]
}

// MOD-EML-11: a contact on exactly one open matter files by address.
func TestMODEML11FilesByAddress(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", msg("Pat Rivera <"+patAddr+">", filingAddr, "Estate question", "When is the hearing?").Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "auto_address")
}

// MOD-EML-12: two contacts on one address, or one contact on two open matters, queue.
func TestMODEML12AmbiguousContacts(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.core.AddContact("Pat", "Rivera-Lee", patAddr)
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Shared address", "x").Raw())
	e.sync()
	e.queued(t, "multiple_contacts")

	e2 := newEnv(t)
	e2.connect(e2.admin, "filing", filingAddr)
	other := e2.core.AddMatter("2026-0050", "Rivera trust", "open", "")
	e2.core.AddParty(other, e2.pat, "client")
	e2.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Which matter", "x").Raw())
	e2.sync()
	e2.queued(t, "multiple_open_matters")
}

// MOD-EML-13: a plus-address token files even from an unknown sender; a surname token
// that fits two open matters queues.
func TestMODEML13PlusToken(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", msg("stranger@unknown.example.test", "filing+2026-0042@"+firmDomain, "Documents", "attached").Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "auto_token")

	e2 := newEnv(t)
	e2.connect(e2.admin, "filing", filingAddr)
	chris := e2.core.AddContact("Chris", "Rivera", "chris@client.example.test")
	m2 := e2.core.AddMatter("2026-0051", "Rivera purchase", "open", "")
	e2.core.AddParty(m2, chris, "client")
	e2.box(filingAddr).Deliver("INBOX", msg("stranger@unknown.example.test", "filing+rivera@"+firmDomain, "Documents", "attached").Raw())
	e2.sync()
	e2.queued(t, "token_multiple_matters")
}

func courtEnv(t *testing.T) *env {
	e := newEnv(t)
	e.core.Settings["court_ai_pick"] = "false"
	e.connect(e.admin, "court", courtAddr)
	return e
}

// MOD-EML-14: 26AB-CV00123-01 never files to 26AB-CV00123, by field or by name.
func TestMODEML14CaseNumberSuffix(t *testing.T) {
	e := courtEnv(t)
	m := e.core.AddMatter("2026-0060", "Rivera v. Smith 26AB-CV00123", "open", "26AB-CV00123")
	e.core.AddParty(m, e.pat, "client")
	e.box(courtAddr).Deliver("INBOX", e.courtNotice("Notice of hearing 26AB-CV00123-01",
		"Case No. 26AB-CV00123-01. Rivera v. Smith. A hearing is set.").Raw())
	e.sync()
	for _, f := range e.filings() {
		if f.MatterID == m.ID {
			t.Fatalf("filed to the unsuffixed matter: %+v", f)
		}
	}
	if len(e.only("unfiled")) != 1 {
		t.Fatalf("rows %+v", e.rows())
	}
}

// MOD-EML-15: case number matches, party text doesn't name the client: queue.
func TestMODEML15PartyMismatch(t *testing.T) {
	e := courtEnv(t)
	m := e.core.AddMatter("2026-0061", "Rivera dispute", "open", "26AB-CV00124")
	e.core.AddParty(m, e.pat, "client")
	e.box(courtAddr).Deliver("INBOX", e.courtNotice("Minute order 26AB-CV00124", "Case 26AB-CV00124. Jones v. Brown. Minute order.").Raw())
	e.sync()
	e.queued(t, "case_number_matched_but_party_name_mismatch")
}

// MOD-EML-16: court mail files to a closed matter by case number; client mail never
// auto-files to a closed matter.
func TestMODEML16ClosedMatters(t *testing.T) {
	e := courtEnv(t)
	m := e.core.AddMatter("2025-0101", "Rivera v. Acme", "closed", "26AB-CV00125")
	e.core.AddParty(m, e.pat, "client")
	e.box(courtAddr).Deliver("INBOX", e.courtNotice("Judgment 26AB-CV00125", "Case 26AB-CV00125. Rivera v. Acme. Judgment entered.").Raw())
	e.sync()
	f := e.filedTo(t, m.ID, "")
	if !strings.HasPrefix(f.How, "auto_court") {
		t.Fatalf("how %s", f.How)
	}

	e2 := newEnv(t)
	e2.connect(e2.admin, "filing", filingAddr)
	lee := e2.core.AddContact("Lee", "Okafor", "lee@client.example.test")
	closed := e2.core.AddMatter("2025-0102", "Okafor lease", "closed", "")
	e2.core.AddParty(closed, lee, "client")
	e2.box(filingAddr).Deliver("INBOX", msg("lee@client.example.test", filingAddr, "Follow up", "x").Raw())
	e2.sync()
	e2.queued(t, "no_open_matter")
}

// MOD-EML-17: the court AI pick: a candidate files by auto_court_ai, a non-candidate
// files nothing, and "none" is not asked again on re-match.
func TestMODEML17CourtAIPick(t *testing.T) {
	setup := func(t *testing.T) (*env, string, string) {
		e := newEnv(t)
		e.connect(e.admin, "court", courtAddr)
		chris := e.core.AddContact("Chris", "Rivera", "chris@client.example.test")
		m2 := e.core.AddMatter("2026-0070", "Rivera purchase", "open", "")
		e.core.AddParty(m2, chris, "client")
		e.box(courtAddr).Deliver("INBOX", e.courtNotice("Notice of filing", "Rivera v. Acme Holdings. A document was filed.").Raw())
		return e, e.m42.ID, m2.ID
	}
	e, _, pick := setup(t)
	e.core.AI = func(map[string]any) map[string]any { return map[string]any{"matter_id": pick} }
	e.sync()
	e.filedTo(t, pick, "auto_court_ai")

	e2, _, _ := setup(t)
	outsider := e2.core.AddMatter("2026-0071", "Unrelated", "open", "")
	e2.core.AI = func(map[string]any) map[string]any { return map[string]any{"matter_id": outsider.ID} }
	e2.sync()
	if n := len(e2.filings()); n != 0 {
		t.Fatalf("filed %d to a non-candidate", n)
	}

	e3, _, _ := setup(t)
	e3.core.AI = func(map[string]any) map[string]any { return map[string]any{"matter_id": ""} }
	e3.sync()
	if e3.core.AICalls != 1 {
		t.Fatalf("AI calls %d", e3.core.AICalls)
	}
	if n := e3.count(`SELECT count(*) FROM messages WHERE ai_declined`); n != 1 {
		t.Fatalf("ai_declined rows %d", n)
	}
	e3.advance(2 * time.Hour)
	e3.sync()
	st, domains, _ := settingsAndDomains(e3)
	if err := e3.m.Syncer.Rematch(context.Background(), st, domains); err != nil {
		t.Fatal(err)
	}
	if e3.core.AICalls != 1 {
		t.Fatalf("asked again: %d calls", e3.core.AICalls)
	}
}

// MOD-EML-18: a reply in a filed thread files there even from an unknown address.
func TestMODEML18Thread(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	first := msg(patAddr, filingAddr, "Estate question", "first")
	e.box(filingAddr).Deliver("INBOX", first.Raw())
	e.sync()
	reply := msg("assistant@other.example.test", filingAddr, "Re: Estate question", "reply")
	reply.InReplyTo = "<" + first.ID + ">"
	reply.References = []string{"<" + first.ID + ">"}
	e.box(filingAddr).Deliver("INBOX", reply.Raw())
	e.sync()
	fs := e.filings()
	hows := map[string]bool{}
	for _, f := range fs {
		if f.MatterID == e.m42.ID {
			hows[f.How] = true
		}
	}
	if len(fs) != 2 || !hows["auto_thread"] || !hows["auto_address"] {
		t.Fatalf("filings %+v", fs)
	}
}

// MOD-EML-19: a walled matter as the only candidate queues, and the row shows no
// matter name to someone off the wall.
func TestMODEML19WalledCandidate(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	lee := e.core.AddContact("Lee", "Okafor", "lee@client.example.test")
	walled := e.core.AddMatter("2026-0077", "Okafor confidential merger", "open", "")
	e.core.AddParty(walled, lee, "client")
	e.core.Wall(walled, e.attorney)
	e.box(filingAddr).Deliver("INBOX", msg("lee@client.example.test", filingAddr, "Merger docs", "x").Raw())
	e.sync()
	r := e.queued(t, "")
	for _, p := range []string{"/api/queue", "/api/queue/" + r.ID, "/unfiled", "/file/" + r.ID} {
		code, b, _ := e.call(http.MethodGet, p, e.staff, "", nil)
		if code >= 500 || strings.Contains(string(b), "Okafor confidential") || strings.Contains(string(b), "2026-0077") {
			t.Fatalf("%s as staff (%d) shows the walled matter: %s", p, code, b)
		}
	}
}

// MOD-EML-20: one message in a personal and a filing account files once.
func TestMODEML20DuplicateAcrossAccounts(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	m := msg(patAddr, filingAddr, "Copied", "x")
	m.Cc = []string{"jordan@firm.example.test"}
	e.box(filingAddr).Deliver("INBOX", m.Raw())
	e.box("jordan@firm.example.test").Deliver("INBOX", m.Raw())
	e.sync()
	e.sync()
	if n := len(e.filings()); n != 1 {
		t.Fatalf("%d filings", n)
	}
	if n := len(e.only("duplicate")); n != 1 {
		t.Fatalf("rows %+v", e.rows())
	}
}
