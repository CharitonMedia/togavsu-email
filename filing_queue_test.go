package email_test

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/fakecore"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/testmail"
	"github.com/CharitonMedia/togavsu-email/internal/webhook"
)

func withParts(m testmail.Msg) testmail.Msg {
	m.HTML = `<p>See the attached <b>papers</b>.</p><img src="cid:logo1">` +
		`<a href="https://court.example.test/case/1">docket</a> <a href="mailto:pat@client.example.test">mail</a>` +
		`<script>alert(1)</script><img src="https://tracker.example.test/p.gif" onerror="x()"><div onclick="y()">click</div>`
	m.Attachments = []testmail.Attachment{
		{Name: "lease.pdf", Type: "application/pdf", Data: testmail.PDF(1)},
		{Name: "photo.jpg", Type: "image/jpeg", Data: []byte("fake-jpeg-bytes-1")},
		{Name: "logo.png", Type: "image/png", Data: []byte("fake-png"), ContentID: "logo1"},
	}
	return m
}

func htmlFile(t *testing.T, files []*fakecore.File) *fakecore.File {
	t.Helper()
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".html") {
			return f
		}
	}
	t.Fatalf("no HTML copy among %d files", len(files))
	return nil
}

// MOD-EML-21: two attachments and an inline image give three files, two links and one
// note tagged email and email-in.
func TestMODEML21FilesLinksAndNote(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", withParts(msg(patAddr, filingAddr, "Lease papers", "See attached.")).Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "auto_address")
	files := e.core.FilesOn(e.m42.ID)
	if len(files) != 3 {
		t.Fatalf("%d files", len(files))
	}
	h := htmlFile(t, files)
	links := e.core.LinksFrom(h.ID)
	if len(links) != 2 {
		t.Fatalf("links %+v", links)
	}
	for _, l := range links {
		if l.Relationship != "attachment" {
			t.Fatalf("link %+v", l)
		}
	}
	notes := e.core.NotesOn(e.m42.ID)
	if len(notes) != 1 || strings.Join(notes[0].Tags, ",") != "email,email-in" {
		t.Fatalf("notes %+v", notes)
	}
	for _, f := range files {
		if f.Origin != "received" {
			t.Fatalf("origin %s", f.Origin)
		}
	}
}

// MOD-EML-22: the HTML copy has no script, handlers or remote src, and keeps links.
func TestMODEML22SanitizedCopy(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", withParts(msg(patAddr, filingAddr, "Lease papers", "See attached.")).Raw())
	e.sync()
	h := string(htmlFile(t, e.core.FilesOn(e.m42.ID)).Versions[0].Data)
	if strings.Contains(strings.ToLower(h), "<script") {
		t.Fatal("script kept")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(h) {
		t.Fatal("event handler kept")
	}
	if regexp.MustCompile(`(?i)src\s*=\s*["']?(https?:)?//`).MatchString(h) {
		t.Fatal("remote src kept")
	}
	if !contains(h, `href="https://court.example.test/case/1"`, `href="mailto:pat@client.example.test"`) {
		t.Fatalf("links lost: %s", h)
	}
}

// MOD-EML-23: a filing interrupted after the second upload ends, once resumed, with
// three files and one note.
func TestMODEML23ResumedFiling(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	// A first pass runs the re-match sweep, so the next one doesn't retry at once.
	e.sync()
	e.box(filingAddr).Deliver("INBOX", withParts(msg(patAddr, filingAddr, "Lease papers", "See attached.")).Raw())
	e.core.FailPublishAt = 3
	e.sync()
	if n := len(e.filings()); n != 0 {
		t.Fatalf("filed despite the interruption")
	}
	if n := len(e.core.FilesOn(e.m42.ID)); n != 2 {
		t.Fatalf("%d files before resuming", n)
	}
	e.advance(2 * time.Hour)
	e.sync()
	e.filedTo(t, e.m42.ID, "")
	if n := len(e.core.FilesOn(e.m42.ID)); n != 3 {
		t.Fatalf("%d files after resuming", n)
	}
	if n := len(e.core.NotesOn(e.m42.ID)); n != 1 {
		t.Fatalf("%d notes", n)
	}
}

// MOD-EML-24: after-filing actions by role, and a delete only after the note.
func TestMODEML24AfterFiling(t *testing.T) {
	e := newEnv(t)
	e.core.Settings["court_ai_pick"] = "false"
	e.connect(e.admin, "filing", filingAddr)
	e.connect(e.admin, "court", courtAddr)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	cm := e.core.AddMatter("2026-0080", "Rivera v. Acme", "open", "26AB-CV00200")
	e.core.AddParty(cm, e.core.AddContact("Chris", "Rivera", "chris@client.example.test"), "client")

	e.sync()
	// The note fails: the filing account's message stays.
	fr := e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "One", "x").Raw())
	e.core.FailNotes = 1
	e.sync()
	if !e.box(filingAddr).Has(fr) || len(e.box(filingAddr).Deleted) != 0 {
		t.Fatal("deleted before the note was written")
	}
	e.advance(2 * time.Hour)
	e.sync()
	if e.box(filingAddr).Has(fr) || len(e.box(filingAddr).Deleted) != 1 || len(e.core.NotesOn(e.m42.ID)) != 1 {
		t.Fatalf("filing account: deleted %v notes %d rows %+v log %s", e.box(filingAddr).Deleted, len(e.core.NotesOn(e.m42.ID)), e.rows(), e.logs.String())
	}

	cr := e.box(courtAddr).Deliver("INBOX", e.courtNotice("Order 26AB-CV00200", "Case 26AB-CV00200. Rivera v. Acme. Order.").Raw())
	pr := e.box("jordan@firm.example.test").Deliver("INBOX", msg(patAddr, "jordan@firm.example.test", "Personal", "x").Raw())
	e.sync()
	cb := e.box(courtAddr)
	if len(cb.Deleted) != 0 || cb.Has(cr) || cb.FolderCount(provider.FiledFolder) != 1 || len(cb.ReadChanges) != 1 {
		t.Fatalf("court: deleted %v moved %v read %v", cb.Deleted, cb.Moved, cb.ReadChanges)
	}
	pb := e.box("jordan@firm.example.test")
	if !pb.Has(pr) || len(pb.Deleted) != 0 || len(pb.Moved) != 0 || len(pb.Labeled) != 1 {
		t.Fatalf("personal: deleted %v moved %v labels %v", pb.Deleted, pb.Moved, pb.Labels(pr))
	}
}

type queueRow struct {
	ID                     string `json:"id"`
	Reason                 string `json:"reason"`
	Error                  string `json:"error"`
	FileWithoutAttachments bool   `json:"file_without_attachments"`
}

// MOD-EML-25: a message gone at filing time stays queued, then files without its
// attachments, naming them in the note.
func TestMODEML25GoneAtFilingTime(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	m := withParts(msg("stranger@unknown.example.test", filingAddr, "Unknown sender", "Please see the lease."))
	r := e.box(filingAddr).Deliver("INBOX", m.Raw())
	e.sync()
	row := e.queued(t, "no_contact")
	e.box(filingAddr).Remove(r)
	code, b, _ := e.call(http.MethodPost, "/api/queue/"+row.ID+"/file", e.staff, "", map[string]any{"matter_id": e.m42.ID})
	if code != http.StatusConflict {
		t.Fatalf("file: %d %s", code, b)
	}
	var q queueRow
	e.json(http.StatusOK, http.MethodGet, "/api/queue/"+row.ID, e.staff, nil, &q)
	if q.Error != "message_not_in_mailbox" || !q.FileWithoutAttachments {
		t.Fatalf("row %+v", q)
	}
	if rs := e.only("unfiled"); len(rs) != 1 {
		t.Fatalf("rows %+v", e.rows())
	}
	e.json(http.StatusOK, http.MethodPost, "/api/queue/"+row.ID+"/file", e.staff, map[string]any{"matter_id": e.m42.ID, "without_attachments": true}, nil)
	notes := e.core.NotesOn(e.m42.ID)
	if len(notes) != 1 || !contains(notes[0].Body, "lease.pdf", "photo.jpg") {
		t.Fatalf("notes %+v", notes)
	}
	if !strings.Contains(string(htmlFile(t, e.core.FilesOn(e.m42.ID)).Versions[0].Data), "papers") {
		t.Fatal("the stored body is not in the copy")
	}
}

// MOD-EML-26: mail from a personal Sent folder files as the firm's and email-out.
func TestMODEML26OutboundFromSent(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	e.box("jordan@firm.example.test").Deliver("Sent", msg("Jordan Attorney <jordan@firm.example.test>", patAddr, "Your estate", "Hi Pat").Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "")
	for _, f := range e.core.FilesOn(e.m42.ID) {
		if f.Origin != "firm" {
			t.Fatalf("origin %s", f.Origin)
		}
	}
	if n := e.core.NotesOn(e.m42.ID); len(n) != 1 || !strings.Contains(strings.Join(n[0].Tags, ","), "email-out") {
		t.Fatalf("notes %+v", n)
	}
}

// MOD-EML-27: include=email.mail through the core for a viewer on and off the wall.
func TestMODEML27IncludeEmailMail(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "One", "x").Raw())
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Two", "x").Raw())
	e.sync()
	get := func(u *fakecore.User) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, e.core.URL()+"/api/v1/matters/"+e.m42.ID+"?include=email.mail", nil)
		req.Header.Set("Authorization", "Bearer "+e.core.Token)
		req.Header.Set("X-Togavsu-Context-Token", e.core.ContextFor(u, ""))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := get(e.staff)
	inc, _ := out["included"].(map[string]any)
	rows, _ := inc["email.mail"].([]any)
	if code != 200 || len(rows) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	r := rows[0].(map[string]any)
	if r["filed_count"].(float64) != 2 || r["last_mail_at"] == nil {
		t.Fatalf("row %v", r)
	}
	e.core.Wall(e.m42, e.attorney)
	if code, _ := get(e.staff); code != http.StatusNotFound {
		t.Fatalf("off the wall: %d", code)
	}
}

func (e *env) badge(u *fakecore.User) int {
	var b struct {
		Count int `json:"count"`
	}
	e.json(http.StatusOK, http.MethodGet, "/badge/unfiled", u, nil, &b)
	return b.Count
}

// MOD-EML-28: filing from the queue as staff files as that user and empties the row.
func TestMODEML28QueueFileAsStaff(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", withParts(msg("stranger@unknown.example.test", filingAddr, "Unknown", "x")).Raw())
	e.sync()
	row := e.queued(t, "")
	if e.badge(e.staff) != 1 {
		t.Fatal("badge before")
	}
	e.json(http.StatusOK, http.MethodPost, "/api/queue/"+row.ID+"/file", e.staff, map[string]any{"matter_id": e.m42.ID}, nil)
	for _, f := range e.core.FilesOn(e.m42.ID) {
		if f.CreatedBy != e.staff.ID {
			t.Fatalf("file by %s", f.CreatedBy)
		}
	}
	if n := e.core.NotesOn(e.m42.ID); len(n) != 1 || n[0].CreatedBy != e.staff.ID {
		t.Fatalf("notes %+v", n)
	}
	if f := e.filings(); len(f) != 1 || f[0].How != "queue" || f[0].FiledBy.String != e.staff.ID {
		t.Fatalf("filings %+v", f)
	}
	var list struct{ Data []queueRow }
	e.json(http.StatusOK, http.MethodGet, "/api/queue", e.staff, nil, &list)
	if len(list.Data) != 0 || e.badge(e.staff) != 0 {
		t.Fatalf("queue %+v", list)
	}
}

// MOD-EML-29: dismiss needs a reason, and a court row is never deleted.
func TestMODEML29Dismiss(t *testing.T) {
	e := newEnv(t)
	e.core.Settings["court_ai_pick"] = "false"
	e.connect(e.admin, "court", courtAddr)
	r := e.box(courtAddr).Deliver("INBOX", e.courtNotice("Unknown case", "Nothing matches here.").Raw())
	e.sync()
	row := e.queued(t, "")
	if code, _, _ := e.call(http.MethodPost, "/api/queue/"+row.ID+"/dismiss", e.staff, "", map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("no reason: %d", code)
	}
	e.json(http.StatusOK, http.MethodPost, "/api/queue/"+row.ID+"/dismiss", e.staff, map[string]any{"reason": "not ours"}, nil)
	b := e.box(courtAddr)
	if !b.Has(r) || len(b.Deleted) != 0 {
		t.Fatalf("court message deleted: %v", b.Deleted)
	}
	if rs := e.only("dismissed"); len(rs) != 1 {
		t.Fatalf("rows %+v", e.rows())
	}
}

// MOD-EML-30: a deferred row stays deferred across syncs and comes back on Restore.
func TestMODEML30DeferRestore(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", msg("stranger@unknown.example.test", filingAddr, "Later", "x").Raw())
	e.sync()
	row := e.queued(t, "")
	e.json(http.StatusOK, http.MethodPost, "/api/queue/"+row.ID+"/defer", e.staff, map[string]any{}, nil)
	for i := 0; i < 3; i++ {
		e.advance(2 * time.Hour)
		e.sync()
	}
	if rs := e.only("deferred"); len(rs) != 1 || len(e.rows()) != 1 {
		t.Fatalf("rows %+v", e.rows())
	}
	e.json(http.StatusOK, http.MethodPost, "/api/queue/"+row.ID+"/restore", e.staff, map[string]any{}, nil)
	e.queued(t, "")
}

// MOD-EML-31: staff can't see, preview or file another user's personal row.
func TestMODEML31PersonalRowsPrivate(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	e.box("jordan@firm.example.test").Deliver("INBOX", msg("stranger@unknown.example.test", "jordan@firm.example.test", "Private", "x").Raw())
	e.sync()
	rows := e.rows()
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	id := rows[0].ID
	for _, c := range []struct{ m, p string }{{"GET", "/api/queue/" + id}, {"GET", "/preview/" + id}, {"GET", "/file/" + id},
		{"GET", "/api/messages/" + id}, {"POST", "/api/queue/" + id + "/file"}} {
		var body any
		if c.m == "POST" {
			body = map[string]any{"matter_id": e.m42.ID}
		}
		if code, _, _ := e.call(c.m, c.p, e.staff, "", body); code != http.StatusNotFound {
			t.Fatalf("%s %s: %d", c.m, c.p, code)
		}
	}
	if n := len(e.filings()); n != 0 {
		t.Fatal("filed")
	}
}

// MOD-EML-32: the preview's CSP and no script.
func TestMODEML32PreviewCSP(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", withParts(msg("stranger@unknown.example.test", filingAddr, "Preview me", "x")).Raw())
	e.sync()
	row := e.queued(t, "")
	code, b, h := e.call(http.MethodGet, "/preview/"+row.ID, e.staff, "", nil)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	if got := h.Get("Content-Security-Policy"); got != "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox" {
		t.Fatalf("CSP %q", got)
	}
	if strings.Contains(strings.ToLower(string(b)), "<script") {
		t.Fatal("script in preview")
	}
}

// MOD-EML-33: a case number change files the waiting court notice on the next
// re-match; the hourly sweep does the same without an event.
func TestMODEML33RematchOnEventAndSweep(t *testing.T) {
	run := func(t *testing.T, event bool) {
		e := newEnv(t)
		e.core.Settings["court_ai_pick"] = "false"
		e.connect(e.admin, "court", courtAddr)
		e.box(courtAddr).Deliver("INBOX", e.courtNotice("Hearing 26AB-CV00123", "Case 26AB-CV00123. Rivera v. Smith. Hearing set.").Raw())
		e.sync()
		e.queued(t, "")
		e.core.SetCaseNumber(e.m42, "26AB-CV00123")
		if event {
			body, _ := json.Marshal(webhook.Event{ID: "ev-" + randomHex(4), Action: "matter.updated", RecordType: "matter", RecordID: e.m42.ID,
				MatterID: e.m42.ID, OccurredAt: e.now().Format(time.RFC3339), Module: "email"})
			if code, b := e.signed("/events", body, nil); code != http.StatusNoContent {
				t.Fatalf("event: %d %s", code, b)
			}
			e.advance(time.Minute)
		} else {
			e.advance(61 * time.Minute)
		}
		if err := e.m.Syncer.Pass(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		e.filedTo(t, e.m42.ID, "auto_court")
	}
	t.Run("event", func(t *testing.T) { run(t, true) })
	t.Run("sweep", func(t *testing.T) { run(t, false) })
}
