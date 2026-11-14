package email_test

// The acceptance suite drives the whole module (email.New with its HTTP handler and
// sync loop) against the fake core (internal/fakecore), the fake mailbox set
// (internal/provider/fake) and a scratch mod_email schema in the togavsu_email
// database (internal/pgtest). No test reaches a real core, mailbox or network
// service; every name, address and matter below is synthetic.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/fakecore"
	"github.com/CharitonMedia/togavsu-email/internal/pgtest"
	"github.com/CharitonMedia/togavsu-email/internal/provider/fake"
	"github.com/CharitonMedia/togavsu-email/internal/testmail"
	"github.com/CharitonMedia/togavsu-email/internal/webhook"
)

const (
	firmDomain = "firm.example.test"
	filingAddr = "filing@firm.example.test"
	courtAddr  = "court@firm.example.test"
	patAddr    = "pat@client.example.test"
)

type env struct {
	t     *testing.T
	db    *pgtest.DB
	core  *fakecore.Core
	boxes *fake.Set
	m     *email.Module
	h     http.Handler
	hook  string
	logs  *syncBuffer

	mu    sync.Mutex
	clock time.Time

	admin, attorney, staff, staff2 *fakecore.User
	pat                            *fakecore.Contact
	m42                            *fakecore.Matter
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

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := pgtest.New(t)
	core := fakecore.New()
	t.Cleanup(core.Close)
	core.Insert = db.Insert
	e := &env{t: t, db: db, core: core, boxes: fake.NewSet(), hook: randomHex(16), logs: &syncBuffer{}, clock: time.Now().Truncate(time.Second)}
	core.IncludeRead = func(ctx context.Context, matterID string) (map[string]any, error) {
		var n int64
		var last sql.NullInt64
		err := db.Module.QueryRowContext(ctx, `SELECT filed_count, last_mail_at FROM matter_mail WHERE matter_id = $1`, matterID).Scan(&n, &last)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		row := map[string]any{"filed_count": n, "last_mail_at": nil}
		if last.Valid {
			row["last_mail_at"] = last.Int64
		}
		return row, nil
	}
	core.Settings["firm_domains"] = firmDomain
	e.admin = core.AddUser("Avery Admin", "admin")
	e.attorney = core.AddUser("Jordan Attorney", "attorney")
	e.staff = core.AddUser("Sam Staff", "staff")
	e.staff2 = core.AddUser("Robin Staff", "staff")
	e.pat = core.AddContact("Pat", "Rivera", patAddr)
	e.m42 = core.AddMatter("2026-0042", "Rivera estate", "open", "")
	core.AddParty(e.m42, e.pat, "client")
	e.m = email.New(email.Config{Core: core.URL(), Token: core.Token, WebhookSecret: e.hook, DB: db.Module,
		Now: e.now, Log: slog.New(slog.NewTextHandler(e.logs, nil)), Factory: e.boxes.Factory(), TempDir: t.TempDir()})
	e.h = e.m.Handler()
	return e
}

func (e *env) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

// call runs one request against the module's handler with a context token for u
// (nil for none) and returns the status and body.
func (e *env) call(method, path string, u *fakecore.User, matterID string, body any) (int, []byte, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if u != nil {
		req.Header.Set("X-Togavsu-Context-Token", e.core.ContextFor(u, matterID))
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes(), w.Header()
}

// json runs call and decodes the answer, failing on an unexpected status.
func (e *env) json(want int, method, path string, u *fakecore.User, body any, out any) {
	e.t.Helper()
	code, b, _ := e.call(method, path, u, "", body)
	if code != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

// signed posts a signed webhook body.
func (e *env) signed(path string, body []byte, header http.Header) (int, []byte) {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SignatureHeader, webhook.Sign(e.hook, e.now(), body))
	for k, v := range header {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

type account struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
	Status  string `json:"status"`
}

// connect connects a fake IMAP account through the accounts API as u.
func (e *env) connect(u *fakecore.User, role, address string) account {
	e.t.Helper()
	body := map[string]any{"role": role, "address": address, "provider": "imap", "imap_host": testmail.Loopback, "imap_port": 993,
		"imap_security": "tls", "smtp_host": testmail.Loopback, "smtp_port": 587, "smtp_security": "starttls", "login": address,
		"password": "pw-" + randomHex(8)}
	if role == "personal" {
		body["owner_user_id"] = u.ID
	}
	var a account
	e.json(http.StatusCreated, http.MethodPost, "/api/accounts", u, body, &a)
	return a
}

func (e *env) box(address string) *fake.Mailbox { return e.boxes.Box(address) }

// sync runs one forced pass of the loop.
func (e *env) sync() {
	e.t.Helper()
	if err := e.m.Syncer.Pass(context.Background(), true); err != nil {
		e.t.Logf("sync pass: %v", err)
	}
}

// msg is a synthetic inbound message.
func msg(from, to, subject, text string) testmail.Msg {
	return testmail.Msg{ID: randomHex(6) + "@mail.example.test", From: from, To: []string{to}, Subject: subject, Text: text,
		Date: time.Now().Add(-time.Hour)}
}

type row struct {
	ID, Status, Reason, Error, Direction string
}

// rows returns every message row in received order.
func (e *env) rows() []row {
	e.t.Helper()
	rs, err := e.db.Module.Query(`SELECT id, status, match_reason, error, direction FROM messages ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.ID, &r.Status, &r.Reason, &r.Error, &r.Direction); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (e *env) only(status string) []row {
	var out []row
	for _, r := range e.rows() {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out
}

type filing struct {
	MessageID, MatterID, How, NoteID, HTMLFileID string
	FiledBy                                      sql.NullString
}

func (e *env) filings() []filing {
	e.t.Helper()
	rs, err := e.db.Module.Query(`SELECT message_id, matter_id, how, note_id, html_file_id, filed_by FROM filings ORDER BY filed_at, message_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []filing
	for rs.Next() {
		var f filing
		if err := rs.Scan(&f.MessageID, &f.MatterID, &f.How, &f.NoteID, &f.HTMLFileID, &f.FiledBy); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.Module.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
