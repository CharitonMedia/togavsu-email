// Package coresuite_test is the module's Postgres acceptance suite on the real composed
// core (pkg/modulekit/coretest): the module's signed package installed by the firm's
// admin, approved on the Modules tab, and served in this process with the API token,
// webhook secret and database login the core handed over. People are the core's own
// signed-in users, walls are the core's, and every core read the module makes goes to the
// core. Mail still comes from the fake mailbox set; no test reaches a real mailbox or
// network service, and every name and address is synthetic.
//
// A criterion that needs a core change the pinned core lacks is written out and skipped
// with the CORE item that unblocks it. Where the reason is an operation the module calls
// (the secret store, mail matching, filing grants, device grants, file origin), the
// skipped body runs the same steps the fake-core suite at the repository root proves.
package coresuite_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/kittest"
	"github.com/CharitonMedia/togavsu-email/internal/provider/fake"
	"github.com/CharitonMedia/togavsu-email/internal/store"
	"github.com/CharitonMedia/togavsu-email/internal/testmail"
	"github.com/CharitonMedia/togavsu-email/internal/webhook"
)

// The core's firm domain is firm.example (coretest); the client contact of the open
// matter is cleo@client.example.
const (
	firmDomain = "firm.example"
	filingAddr = "filing@firm.example"
	courtAddr  = "court@firm.example"
	clientAddr = "cleo@client.example"
)

type kcore struct {
	*coretest.Core
	t     *testing.T
	m     *email.Module
	boxes *fake.Set
	logs  *syncBuffer
	// files is the package as the pinned core installs it (kittest.Installable).
	files map[string][]byte
	pub   ed25519.PublicKey
	key   ed25519.PrivateKey
}

// newCore starts the composed core, installs the module's signed package as the admin
// and approves every permission; the deployment builds the module from the secrets the
// core handed over.
func newCore(t *testing.T) *kcore { return newCoreWith(t) }

// newCoreWith declines the scopes named and approves the rest.
func newCoreWith(t *testing.T, decline ...string) *kcore {
	t.Helper()
	k := install(t)
	if len(decline) == 0 {
		k.ApproveAll(email.ID)
	} else if r := k.decide(decline...); r.Code != http.StatusOK {
		t.Fatalf("permissions: %d %s", r.Code, r.Body)
	}
	if k.m == nil {
		t.Fatal("approval deployed no module")
	}
	return k
}

// install starts the core and installs the signed package, awaiting approval.
func install(t *testing.T) *kcore {
	t.Helper()
	k := &kcore{Core: coretest.New(t), t: t, boxes: fake.NewSet(), logs: &syncBuffer{}}
	k.Serve(email.ID, func(m coretest.Module) http.Handler {
		k.m = email.New(email.Config{Core: m.CoreURL, Token: m.Token, WebhookSecret: m.WebhookSecret, DB: m.DB,
			Log: slog.New(slog.NewTextHandler(k.logs, nil)), Factory: k.boxes.Factory(), TempDir: t.TempDir()})
		return k.m.Handler()
	})
	k.files, _ = kittest.Installable(t)
	pub, priv := kittest.Key(t)
	k.pub, k.key = pub, priv
	k.TrustKey("Email module test key", pub)
	k.Install(k.signed())
	return k
}

// signed is the package signed with the suite's key.
func (k *kcore) signed() []byte {
	k.t.Helper()
	b, err := modulekit.Build(k.key, k.files)
	if err != nil {
		k.t.Fatal(err)
	}
	return b
}

// decide approves every permission in the manifest except the scopes named, which it
// declines, through the permissions API as the admin.
func (k *kcore) decide(decline ...string) coretest.Resp {
	k.t.Helper()
	m, err := modulekit.ParseManifest(k.files[modulekit.ManifestFile])
	if err != nil {
		k.t.Fatal(err)
	}
	no := map[string]bool{}
	for _, s := range decline {
		no[s] = true
	}
	var ds []map[string]string
	for _, p := range m.Permissions {
		d := "approve"
		if no[p.Scope] {
			d = "decline"
		}
		ds = append(ds, map[string]string{"scope": p.Scope, "decision": d})
	}
	return k.Admin.Do(http.MethodPost, "/api/v1/modules/"+email.ID+"/permissions", map[string]any{"decisions": ds})
}

// user is the core user of role who.
func (k *kcore) user(who string) *coretest.User { return k.As(who) }

// token is a context token the core issues to who for the module (matter "" for none).
func (k *kcore) token(who, matter string) string { return k.ContextFor(who, email.ID, matter) }

// viewer is a core call as who, the way the module makes it from who's frame.
func (k *kcore) viewer(who string) coreapi.Auth { return coreapi.Viewer(k.token(who, "")) }

// page requests a module path through the core's /m/email/ route as who, with who's
// context token, as the frame and its scripts do.
func (k *kcore) page(who, method, path string, body any) coretest.Resp {
	k.t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return k.user(who).Do(method, "/m/"+email.ID+path+sep+"context="+url.QueryEscape(k.token(who, "")), body)
}

// direct calls the module's own server (the deployment), as the core's webhooks reach
// it, and returns the response with its headers.
func (k *kcore) direct(method, path string, body []byte, header http.Header) (*http.Response, []byte) {
	k.t.Helper()
	req, err := http.NewRequest(method, strings.TrimRight(k.Runtime.BaseURL(email.ID), "/")+path, bytes.NewReader(body))
	if err != nil {
		k.t.Fatal(err)
	}
	for h, v := range header {
		req.Header[h] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// signedPost posts body to the module signed with the deployment's webhook secret.
func (k *kcore) signedPost(path string, body []byte, header http.Header) (*http.Response, []byte) {
	k.t.Helper()
	h := http.Header{"Content-Type": {"application/json"}}
	h.Set(webhook.SignatureHeader, webhook.Sign(k.Last(email.ID).Secrets["togavsu_webhook_secret"], time.Now(), body))
	for n, v := range header {
		h[n] = v
	}
	return k.direct(http.MethodPost, path, body, h)
}

// --- the module's own rows ---

func (k *kcore) store() *store.Store { return k.m.Store }

// account stores an account in the module's schema as the module does after a connect.
// Its sealed credentials would live in the core's secret store (CORE-06), which the
// pinned core lacks, so only tests that never open the mailbox use a stored account.
func (k *kcore) account(role, address string, owner *coretest.User, mod func(*store.Account)) store.Account {
	k.t.Helper()
	a := store.Account{Provider: "imap", Role: role, Address: address, CreatedBy: k.Admin.ID, Login: address,
		IMAPHost: testmail.Loopback, IMAPPort: 993, SMTPHost: testmail.Loopback, SMTPPort: 587, SyncFrom: time.Now().Add(-24 * time.Hour).UnixMilli()}
	switch role {
	case "personal":
		a.AfterFile, a.OwnerUserID, a.CanSend = "label", owner.ID, true
	case "filing":
		a.AfterFile, a.CanSend, a.PlusAddressing = "delete", true, true
	case "court":
		a.AfterFile = "move"
	}
	if mod != nil {
		mod(&a)
	}
	if err := k.store().InsertAccount(context.Background(), &a); err != nil {
		k.t.Fatal(err)
	}
	return a
}

// message stores a message row on a (in its folder of kind, when set) with status.
func (k *kcore) message(a store.Account, kind, status, from, subject, text string, mod func(*store.Message)) store.Message {
	k.t.Helper()
	ctx := context.Background()
	m := store.Message{AccountID: a.ID, ProviderRef: "seed:" + randomHex(8), RFCMessageID: randomHex(6) + "@mail.example.test",
		Direction: "inbound", FromHeader: from, FromAddr: from, ToAddrs: []string{a.Address}, Subject: subject, BodyText: text,
		Snippet: text, Status: status, ReceivedAt: time.Now().UnixMilli()}
	if kind != "" {
		f, err := k.store().UpsertFolder(ctx, a.ID, strings.ToUpper(kind), kind, kind)
		if err != nil {
			k.t.Fatal(err)
		}
		m.FolderID = f.ID
	}
	if mod != nil {
		mod(&m)
	}
	m.ThreadKey = m.RFCMessageID
	if ok, err := k.store().Claim(ctx, &m); err != nil || !ok {
		k.t.Fatalf("claim: %v %v", ok, err)
	}
	got, err := k.store().Message(ctx, m.ID)
	if err != nil {
		k.t.Fatal(err)
	}
	return got
}

// filed records m as filed to matter with a real core note and file made by who.
func (k *kcore) filed(m store.Message, who, matter string) store.Filing {
	k.t.Helper()
	ctx := context.Background()
	note, err := k.m.Core.CreateNote(ctx, k.viewer(who), "seed-note-"+m.ID, matter, "Email: "+m.Subject, "Synthetic filing.", []string{"email", "email-in"})
	if err != nil {
		k.t.Fatal(err)
	}
	f := store.Filing{MessageID: m.ID, MatterID: matter, RFCMessageID: m.RFCMessageID, NoteID: note.ID,
		HTMLFileID: k.coreFile(who, matter, m.Subject+".html", []byte("<p>"+m.Subject+"</p>")), How: "auto_address"}
	if err := k.store().RecordFiling(ctx, f, m.Direction); err != nil {
		k.t.Fatal(err)
	}
	got, err := k.store().Filing(ctx, m.ID)
	if err != nil {
		k.t.Fatal(err)
	}
	return got
}

// coreFile uploads data to matter as who through the core's file API and returns the
// file id.
func (k *kcore) coreFile(who, matter, name string, data []byte) string {
	k.t.Helper()
	u := k.user(who)
	sum := sha256.Sum256(data)
	up := u.Must(http.StatusCreated, http.MethodPost, "/api/v1/file-uploads", map[string]any{"file_name": name, "matter_id": matter,
		"media_type": "text/html", "size_bytes": len(data), "sha256": hex.EncodeToString(sum[:])})
	id := str(k.t, up.JSON, "id")
	if r := u.Raw(http.MethodPut, "/api/v1/file-uploads/"+id, data, "Content-Type", "application/octet-stream",
		"Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))); r.Code >= 300 {
		k.t.Fatalf("chunk: %d %s", r.Code, r.Body)
	}
	if r := u.Do(http.MethodPost, "/api/v1/file-uploads/"+id+"/complete", nil); r.Code >= 300 {
		k.t.Fatalf("complete: %d %s", r.Code, r.Body)
	}
	f := u.Do(http.MethodPost, "/api/v1/files", map[string]any{"upload_id": id, "matter_id": matter, "name": name})
	if f.Code >= 300 {
		k.t.Fatalf("publish: %d %s", f.Code, f.Body)
	}
	return str(k.t, f.JSON, "id")
}

// matterOf reads a matter as who.
func (k *kcore) matterOf(who, id string) map[string]any {
	k.t.Helper()
	return k.user(who).Must(http.StatusOK, http.MethodGet, "/api/v1/matters/"+id, nil).JSON
}

func (k *kcore) count(q string, args ...any) int {
	k.t.Helper()
	var n int
	if err := k.m.Store.DB.QueryRow(q, args...).Scan(&n); err != nil {
		k.t.Fatal(err)
	}
	return n
}

// --- skipped criteria's steps (they run once the CORE items land) ---

type account struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
	Status  string `json:"status"`

	password string // the synthetic password connect sent
}

// connect connects a fake IMAP account through the accounts API as who; the password
// goes to the core's secret store (CORE-06).
func (k *kcore) connect(who, role, address string) account {
	k.t.Helper()
	body := map[string]any{"role": role, "address": address, "provider": "imap", "imap_host": testmail.Loopback, "imap_port": 993,
		"imap_security": "tls", "smtp_host": testmail.Loopback, "smtp_port": 587, "smtp_security": "starttls", "login": address,
		"password": "pw-" + randomHex(8)}
	if role == "personal" {
		body["owner_user_id"] = k.user(who).ID
	}
	r := k.page(who, http.MethodPost, "/api/accounts", body)
	if r.Code != http.StatusCreated {
		k.t.Fatalf("connect %s: %d %s", address, r.Code, r.Body)
	}
	var a account
	decode(k.t, r.Body, &a)
	a.password = body["password"].(string)
	return a
}

func (k *kcore) box(address string) *fake.Mailbox { return k.boxes.Box(address) }

// sync runs one forced pass of the module's loop.
func (k *kcore) sync() {
	k.t.Helper()
	if err := k.m.Syncer.Pass(context.Background(), true); err != nil {
		k.t.Logf("sync pass: %v", err)
	}
}

func (k *kcore) filings() []store.Filing {
	k.t.Helper()
	var out []store.Filing
	rows, err := k.m.Store.DB.Query(`SELECT message_id, matter_id, how, note_id, html_file_id FROM filings ORDER BY filed_at, message_id`)
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var f store.Filing
		if err := rows.Scan(&f.MessageID, &f.MatterID, &f.How, &f.NoteID, &f.HTMLFileID); err != nil {
			k.t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func (k *kcore) statuses() map[string]int {
	k.t.Helper()
	out := map[string]int{}
	rows, err := k.m.Store.DB.Query(`SELECT status, match_reason FROM messages`)
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s, r string
		if err := rows.Scan(&s, &r); err != nil {
			k.t.Fatal(err)
		}
		out[s]++
		out[s+":"+r]++
	}
	return out
}

// coreFiles lists a matter's files on the core as the admin.
func (k *kcore) coreFiles(matter string) []map[string]any {
	k.t.Helper()
	return list(k.t, k.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/files?limit=100&matter_id="+url.QueryEscape(matter), nil).Body)
}

// coreNotes lists a matter's notes on the core as the admin.
func (k *kcore) coreNotes(matter string) []map[string]any {
	k.t.Helper()
	return list(k.t, k.Admin.Must(http.StatusOK, http.MethodGet, "/api/v1/notes?limit=100&matter_id="+url.QueryEscape(matter), nil).Body)
}

// inbound is a synthetic message.
func inbound(from, to, subject, text string) testmail.Msg {
	return testmail.Msg{ID: randomHex(6) + "@mail.example.test", From: from, To: []string{to}, Subject: subject, Text: text,
		Date: time.Now().Add(-time.Hour)}
}

// --- small helpers ---

func list(t *testing.T, body string) []map[string]any {
	t.Helper()
	var page struct {
		Data []map[string]any `json:"data"`
	}
	decode(t, body, &page)
	return page.Data
}

func decode(t *testing.T, body string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok || s == "" {
		t.Fatalf("no %s in %v", key, m)
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// notesGap is why a filing cannot be stored on the pinned core: the module's
// filings.note_id references core.notes (SPEC section on the schema), but the core keeps
// notes only in practice_records and projects matters, contacts, areas, counties and
// referral sources into their tables, never notes. No CORE item covers it.
const notesGap = "needs the core to write notes to core.notes: POST /api/v1/notes answers 201 but core.notes stays empty, " +
	"so filings.note_id REFERENCES core.notes cannot be met (no CORE item covers it)"

// notesDurable reports whether a note the module creates through the core lands in
// core.notes, which every filing's foreign key needs.
func (k *kcore) notesDurable() bool {
	k.t.Helper()
	note, err := k.m.Core.CreateNote(context.Background(), k.viewer(coretest.Attorney), "probe-note-"+randomHex(4), k.Open,
		"Probe", "Synthetic probe note.", []string{"email"})
	if err != nil {
		k.t.Fatal(err)
	}
	return k.Count(`SELECT count(*) FROM core.notes WHERE id = $1`, note.ID) == 1
}

// requireFilings skips t while the core cannot hold a filing's note.
func (k *kcore) requireFilings() {
	k.t.Helper()
	if !k.notesDurable() {
		k.t.Skip(notesGap)
	}
}
