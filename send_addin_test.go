package email_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gomail "github.com/emersion/go-message/mail"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/fakecore"
	"github.com/CharitonMedia/togavsu-email/internal/server"
)

const jordanAddr = "jordan@firm.example.test"

func (e *env) sendAs(u *fakecore.User, body map[string]any) (int, map[string]any) {
	e.t.Helper()
	code, b, _ := e.call(http.MethodPost, "/api/send", u, "", body)
	var out map[string]any
	json.Unmarshal(b, &out)
	return code, out
}

// MOD-EML-34: an invalid address is 400 and nothing is sent.
func TestMODEML34InvalidAddress(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", jordanAddr)
	code, _ := e.sendAs(e.attorney, map[string]any{"to": "not-an-address", "subject": "Hi", "body": "Hello"})
	if code != http.StatusBadRequest || len(e.box(jordanAddr).SentMessages()) != 0 {
		t.Fatalf("%d, sent %d", code, len(e.box(jordanAddr).SentMessages()))
	}
}

func attachmentSums(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h, ok := p.Header.(*gomail.AttachmentHeader); ok {
			name, _ := h.Filename()
			b, _ := io.ReadAll(p.Body)
			s := sha256.Sum256(b)
			out[name] = hex.EncodeToString(s[:])
		}
	}
	return out
}

// MOD-EML-35: quick email with a matter and a matter file sends once and files one
// HTML copy and one note; the attachment is the file version's bytes.
func TestMODEML35QuickEmailWithMatterFile(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", jordanAddr)
	f := e.core.AddFile(e.m42, "will-draft.pdf", "application/pdf", []byte("%PDF-1.4 synthetic will draft"), e.attorney)
	before := len(e.core.FilesOn(e.m42.ID))
	code, out := e.sendAs(e.attorney, map[string]any{"to": patAddr, "subject": "Your will draft", "body": "Attached.", "matter_id": e.m42.ID,
		"file_ids": []string{f.ID}})
	if code != http.StatusOK || out["status"] != "sent" {
		t.Fatalf("%d %v", code, out)
	}
	sent := e.box(jordanAddr).SentMessages()
	if len(sent) != 1 {
		t.Fatalf("sent %d", len(sent))
	}
	if got := attachmentSums(t, sent[0].Raw)["will-draft.pdf"]; got != f.Versions[0].SHA256 {
		t.Fatalf("attachment sha %s, file %s", got, f.Versions[0].SHA256)
	}
	files := e.core.FilesOn(e.m42.ID)
	if len(files)-before != 1 {
		t.Fatalf("files %d -> %d", before, len(files))
	}
	if links := e.core.LinksFrom(htmlFile(t, files).ID); len(links) != 1 || links[0].To != f.ID {
		t.Fatalf("the copy links %+v, want the matter file", links)
	}
	if n := e.core.NotesOn(e.m42.ID); len(n) != 1 || !strings.Contains(strings.Join(n[0].Tags, ","), "email-out") {
		t.Fatalf("notes %+v", n)
	}
}

// MOD-EML-36: a walled matter is not found and nothing is sent.
func TestMODEML36WalledMatter(t *testing.T) {
	e := newEnv(t)
	e.connect(e.staff, "personal", "sam@firm.example.test")
	e.core.Wall(e.m42, e.attorney)
	code, _ := e.sendAs(e.staff, map[string]any{"to": patAddr, "subject": "Hi", "body": "Hello", "matter_id": e.m42.ID})
	if code != http.StatusNotFound || len(e.box("sam@firm.example.test").SentMessages()) != 0 {
		t.Fatalf("%d", code)
	}
}

// MOD-EML-37: a provider failure records failed, writes no note and answers 502.
func TestMODEML37ProviderFailure(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", jordanAddr)
	e.box(jordanAddr).FailSend = errors.New("fake: relay refused")
	code, _ := e.sendAs(e.attorney, map[string]any{"to": patAddr, "subject": "Hi", "body": "Hello", "matter_id": e.m42.ID})
	if code != http.StatusBadGateway {
		t.Fatalf("%d", code)
	}
	var status, errText string
	if err := e.db.Module.QueryRow(`SELECT status, error FROM outbound`).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(errText, "relay refused") || len(e.core.NotesOn(e.m42.ID)) != 0 {
		t.Fatalf("%s %q notes %d", status, errText, len(e.core.NotesOn(e.m42.ID)))
	}
}

// MOD-EML-38: a reply threads to the original and its copy files to the same matter.
func TestMODEML38Reply(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", jordanAddr)
	orig := msg(patAddr, jordanAddr, "Estate question", "Is the will ready?")
	e.box(jordanAddr).Deliver("INBOX", orig.Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "auto_address")
	id := e.rows()[0].ID
	code, out := e.sendAs(e.attorney, map[string]any{"kind": "reply", "reply_to": id, "to": patAddr, "subject": "Re: Estate question", "body": "Yes."})
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	sent := e.box(jordanAddr).SentMessages()
	if len(sent) != 1 || !strings.Contains(string(sent[0].Raw), "In-Reply-To: <"+orig.ID+">") {
		t.Fatalf("sent %q", sent)
	}
	fs := e.filings()
	if len(fs) != 2 || fs[0].MatterID != e.m42.ID || fs[1].MatterID != e.m42.ID {
		t.Fatalf("filings %+v", fs)
	}
}

func (e *env) addin(method, path, device string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(server.DeviceHeader, device)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// MOD-EML-39: a revoked device grant is 401 and files nothing.
func TestMODEML39RevokedDevice(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", jordanAddr)
	dev := e.core.NewDevice(e.attorney)
	e.core.RevokeDevice(dev)
	raw := msg(patAddr, jordanAddr, "From Outlook", "x").Raw()
	code, _ := e.addin(http.MethodPost, "/addin/api/file", dev, map[string]any{"matter_id": e.m42.ID, "raw_base64": base64.StdEncoding.EncodeToString(raw)})
	if code != http.StatusUnauthorized || len(e.filings()) != 0 {
		t.Fatalf("%d", code)
	}
}

// MOD-EML-40: filing the same item twice from the add-in makes one filing.
func TestMODEML40AddinTwice(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", jordanAddr)
	dev := e.core.NewDevice(e.attorney)
	raw := withParts(msg(patAddr, jordanAddr, "From Outlook", "x")).Raw()
	for i := 0; i < 2; i++ {
		code, b := e.addin(http.MethodPost, "/addin/api/file", dev, map[string]any{"matter_id": e.m42.ID, "raw_base64": base64.StdEncoding.EncodeToString(raw)})
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("try %d: %d %s", i, code, b)
		}
	}
	if fs := e.filings(); len(fs) != 1 || fs[0].How != "addin" {
		t.Fatalf("filings %+v", fs)
	}
	if n := len(e.core.NotesOn(e.m42.ID)); n != 1 {
		t.Fatalf("%d notes", n)
	}
}

// MOD-EML-41: the add-in suggests a walled matter only to a user on the wall.
func TestMODEML41AddinWalledSuggestion(t *testing.T) {
	e := newEnv(t)
	e.core.Wall(e.m42, e.attorney)
	body := map[string]any{"from": patAddr, "to": []string{jordanAddr}, "subject": "Question"}
	code, b := e.addin(http.MethodPost, "/addin/api/suggest", e.core.NewDevice(e.attorney), body)
	if code != 200 || !strings.Contains(string(b), e.m42.ID) {
		t.Fatalf("on the wall: %d %s", code, b)
	}
	code, b = e.addin(http.MethodPost, "/addin/api/suggest", e.core.NewDevice(e.staff), body)
	if code != 200 || strings.Contains(string(b), e.m42.ID) || strings.Contains(string(b), "Rivera estate") {
		t.Fatalf("off the wall: %d %s", code, b)
	}
}

func systemBody(id string) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "kind": "password_reset", "to": "robin@firm.example.test",
		"subject": "Reset your password", "text": "Use the link to reset your password."})
	return b
}

// MOD-EML-42 and 45: a signed system mail sends once; a replay answers the same id;
// the outbound row keeps no body.
func TestMODEML42And45SystemMail(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.admin, "filing", filingAddr)
	e.json(http.StatusOK, http.MethodPost, "/api/accounts/"+a.ID+"/system-mail", e.admin, map[string]any{}, nil)
	id := "mail-" + randomHex(4)
	h := http.Header{"Idempotency-Key": {id}}
	code, b1 := e.signed("/system-mail", systemBody(id), h)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, b1)
	}
	code, b2 := e.signed("/system-mail", systemBody(id), h)
	if code != http.StatusAccepted || !bytes.Equal(b1, b2) {
		t.Fatalf("replay %d %s vs %s", code, b2, b1)
	}
	if n := len(e.box(filingAddr).SentMessages()); n != 1 {
		t.Fatalf("sent %d", n)
	}
	var body string
	if err := e.db.Module.QueryRow(`SELECT body_text FROM outbound WHERE kind = 'system'`).Scan(&body); err != nil || body != "" {
		t.Fatalf("body %q %v", body, err)
	}
}

// MOD-EML-43: a bad signature is 401 and sends nothing.
func TestMODEML43BadSignature(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.admin, "filing", filingAddr)
	e.json(http.StatusOK, http.MethodPost, "/api/accounts/"+a.ID+"/system-mail", e.admin, map[string]any{}, nil)
	for _, sig := range []string{"", "t=1,v1=deadbeef"} {
		req := httptest.NewRequest(http.MethodPost, "/system-mail", bytes.NewReader(systemBody("x1")))
		if sig != "" {
			req.Header.Set("X-Togavsu-Signature", sig)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%q: %d", sig, w.Code)
		}
	}
	if n := len(e.box(filingAddr).SentMessages()); n != 0 {
		t.Fatalf("sent %d", n)
	}
}

// MOD-EML-44, module side: no system mail account answers 503 not_configured. The core
// falling back to SMTP needs CC-1 (CORE-05).
func TestMODEML44NotConfigured(t *testing.T) {
	e := newEnv(t)
	code, b := e.signed("/system-mail", systemBody("y1"), http.Header{"Idempotency-Key": {"y1"}})
	if code != http.StatusServiceUnavailable || !strings.Contains(string(b), "not_configured") {
		t.Fatalf("%d %s", code, b)
	}
}

// MOD-EML-46: search never shows walled filings or another user's personal mail.
func TestMODEML46Search(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.connect(e.attorney, "personal", jordanAddr)
	lee := e.core.AddContact("Lee", "Okafor", "lee@client.example.test")
	walled := e.core.AddMatter("2026-0077", "Okafor merger", "open", "")
	e.core.AddParty(walled, lee, "client")
	e.core.Wall(walled, e.attorney)
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Contract open", "contract").Raw())
	e.box(filingAddr).Deliver("INBOX", msg("lee@client.example.test", filingAddr, "Contract walled", "contract").Raw())
	e.box(jordanAddr).Deliver("INBOX", msg("friend@else.example.test", jordanAddr, "Contract personal", "contract").Raw())
	e.sync()
	// The attorney on the wall files the walled one from the queue.
	for _, r := range e.only("unfiled") {
		var q struct {
			Subject string `json:"subject"`
		}
		e.json(http.StatusOK, http.MethodGet, "/api/queue/"+r.ID, e.attorney, nil, &q)
		if q.Subject == "Contract walled" {
			e.json(http.StatusOK, http.MethodPost, "/api/queue/"+r.ID+"/file", e.attorney, map[string]any{"matter_id": walled.ID}, nil)
		}
	}
	_, b, _ := e.call(http.MethodGet, "/api/search?q=contract", e.staff, "", nil)
	s := string(b)
	if !strings.Contains(s, "Contract open") || strings.Contains(s, "Contract walled") || strings.Contains(s, "Contract personal") {
		t.Fatalf("staff search: %s", s)
	}
	_, b, _ = e.call(http.MethodGet, "/api/search?q=contract", e.attorney, "", nil)
	if s := string(b); !contains(s, "Contract open", "Contract walled", "Contract personal") {
		t.Fatalf("attorney search: %s", s)
	}
}

// MOD-EML-47: two runs inside one interval make one summary of 3; a quiet interval
// sends none.
func TestMODEML47Summary(t *testing.T) {
	e := newEnv(t)
	e.core.Settings["summary_recipients"] = "partner@firm.example.test"
	e.core.Settings["summary_interval_minutes"] = "60"
	a := e.connect(e.admin, "filing", filingAddr)
	e.json(http.StatusOK, http.MethodPost, "/api/accounts/"+a.ID+"/default-send", e.admin, map[string]any{}, nil)
	b := e.box(filingAddr)
	b.Deliver("INBOX", msg(patAddr, filingAddr, "First", "x").Raw())
	b.Deliver("INBOX", msg(patAddr, filingAddr, "Second", "x").Raw())
	e.sync()
	e.advance(20 * time.Minute)
	b.Deliver("INBOX", msg(patAddr, filingAddr, "Third", "x").Raw())
	e.sync()
	if n := len(b.SentMessages()); n != 0 {
		t.Fatalf("summary inside the interval: %d", n)
	}
	e.advance(45 * time.Minute)
	e.sync()
	e.advance(30 * time.Minute)
	e.sync()
	sent := b.SentMessages()
	if len(sent) != 1 || !contains(string(sent[0].Raw), "3 emails filed", "First", "Second", "Third") {
		t.Fatalf("summaries %d", len(sent))
	}
	e.advance(2 * time.Hour)
	e.sync()
	if n := len(b.SentMessages()); n != 1 {
		t.Fatalf("a quiet interval sent %d", n)
	}
}

// MOD-EML-48: the widgets' targets are the inbox tab with its folder and the unfiled
// tab with its account, and each widget and tab honors the parameter.
func TestMODEML48WidgetTargets(t *testing.T) {
	e := newEnv(t)
	files, err := email.Files()
	if err != nil {
		t.Fatal(err)
	}
	man := string(files["togavsu-module.yaml"])
	for _, want := range []string{"path: /widget/inbox", "path: /widget/unfiled"} {
		i := strings.Index(man, want)
		if i < 0 {
			t.Fatalf("no %s in the manifest", want)
		}
		block := man[i:]
		if j := strings.Index(block[1:], "  - id:"); j > 0 {
			block = block[:j]
		}
		target := "tab: inbox\n      params: [folder]"
		if strings.Contains(want, "unfiled") {
			target = "tab: unfiled\n      params: [account_id]"
		}
		if !strings.Contains(block, target) {
			t.Fatalf("widget %s target: %s", want, block)
		}
	}
	f := e.connect(e.admin, "filing", filingAddr)
	c := e.connect(e.admin, "court", courtAddr)
	e.box(filingAddr).Deliver("INBOX", msg("stranger@unknown.example.test", filingAddr, "Filing side", "x").Raw())
	e.box(courtAddr).Deliver("INBOX", e.courtNotice("Court side", "nothing").Raw())
	e.core.Settings["court_ai_pick"] = "false"
	e.sync()
	for _, p := range []string{"/widget/unfiled?account_id=" + f.ID, "/unfiled?account_id=" + f.ID} {
		_, b, _ := e.call(http.MethodGet, p, e.admin, "", nil)
		if !strings.Contains(string(b), "Filing side") || strings.Contains(string(b), "Court side") {
			t.Fatalf("%s: %s", p, b)
		}
	}
	_, b, _ := e.call(http.MethodGet, "/widget/unfiled?account_id="+c.ID, e.admin, "", nil)
	if strings.Contains(string(b), "Filing side") {
		t.Fatalf("court widget: %s", b)
	}
	e.connect(e.attorney, "personal", jordanAddr)
	e.box(jordanAddr).Deliver("INBOX", msg("friend@else.example.test", jordanAddr, "In the inbox", "x").Raw())
	e.box(jordanAddr).Deliver("Sent", msg(jordanAddr, "friend@else.example.test", "In sent", "x").Raw())
	e.sync()
	for _, p := range []string{"/widget/inbox?folder=sent", "/inbox?folder=sent"} {
		_, b, _ = e.call(http.MethodGet, p, e.attorney, "", nil)
		if !strings.Contains(string(b), "In sent") || strings.Contains(string(b), "In the inbox") {
			t.Fatalf("%s: %s", p, b)
		}
	}
	_, b, _ = e.call(http.MethodGet, "/widget/inbox", e.attorney, "", nil)
	if strings.Contains(string(b), "In sent") || !strings.Contains(string(b), "In the inbox") {
		t.Fatalf("inbox widget: %s", b)
	}
}
