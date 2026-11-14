package imapsmtp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/testmail"
)

func secret() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type rig struct {
	t      *testing.T
	a      *Adapter
	user   *imapmemserver.User
	addr   string
	client *tls.Config
	login  string
	pass   string
	smtp   *testmail.SMTPServer
}

// newRig starts the go-imap in-memory server on 127.0.0.1 with the given
// capabilities, implicit TLS or STARTTLS, and a local SMTP listener.
func newRig(t *testing.T, caps imap.CapSet, starttls bool) *rig {
	t.Helper()
	scfg, ccfg := testmail.Cert()
	r := &rig{t: t, client: ccfg, login: "clerk@firm.test", pass: secret()}
	mem := imapmemserver.New()
	r.user = imapmemserver.NewUser(r.login, r.pass)
	for _, mb := range []string{"INBOX", "Sent", "Trash", "Drafts", "Clients"} {
		if err := r.user.Create(mb, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(r.user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:      caps,
		TLSConfig: scfg,
		Logger:    discard{},
	})
	ln, err := net.Listen("tcp", testmail.Loopback+":0")
	if err != nil {
		t.Fatal(err)
	}
	if !starttls {
		ln = tls.NewListener(ln, scfg)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	r.addr = ln.Addr().String()
	r.smtp = testmail.NewSMTP(scfg, false, true)
	r.smtp.User, r.smtp.Password = r.login, r.pass
	t.Cleanup(r.smtp.Close)
	sec := "tls"
	if starttls {
		sec = "starttls"
	}
	port := ln.Addr().(*net.TCPAddr).Port
	c := provider.Config{AccountID: "a1", Provider: "imap", Address: r.login, IMAPHost: testmail.Loopback, IMAPPort: port,
		IMAPSecurity: sec, SMTPHost: testmail.Loopback, SMTPPort: r.smtp.Port, SMTPSecurity: "starttls", Login: r.login, Password: r.pass}
	testmail.MustLoopback(c.IMAPHost)
	testmail.MustLoopback(c.SMTPHost)
	r.a = New(c)
	r.a.TLSConfig = ccfg
	return r
}

type discard struct{}

func (discard) Printf(string, ...any) {}

func (r *rig) raw(n int, subject string) []byte {
	return testmail.Msg{From: "Pat Client <pat@client.test>", To: []string{r.login}, Subject: subject,
		ID: "m" + strings.Repeat("x", n) + "@client.test", Date: time.Date(2026, 3, n, 9, 0, 0, 0, time.UTC),
		Text: "Body " + subject}.Raw()
}

// direct opens a separate client to inspect or change the mailbox.
func (r *rig) direct() *imapclient.Client {
	r.t.Helper()
	c, err := imapclient.DialTLS(r.addr, &imapclient.Options{TLSConfig: r.client})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := c.Login(r.login, r.pass).Wait(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { c.Close() })
	return c
}

func (r *rig) put(c *imapclient.Client, mb string, raw []byte) {
	r.t.Helper()
	// The internal date is the Date header's, as on a server that received it then.
	var when time.Time
	if i := strings.Index(string(raw), "Date: "); i >= 0 {
		line := string(raw[i+6:])
		line = line[:strings.Index(line, "\r\n")]
		when, _ = time.Parse(time.RFC1123Z, line)
	}
	ap := c.Append(mb, int64(len(raw)), &imap.AppendOptions{Time: when})
	ap.Write(raw)
	ap.Close()
	if _, err := ap.Wait(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) flags(c *imapclient.Client, mb string) [][]imap.Flag {
	r.t.Helper()
	if _, err := c.Select(mb, nil).Wait(); err != nil {
		r.t.Fatal(err)
	}
	ms, err := c.Fetch(imap.SeqSet{imap.SeqRange{Start: 1, Stop: 0}}, &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		if strings.Contains(err.Error(), "No matching") || strings.Contains(err.Error(), "Invalid") {
			return nil
		}
		r.t.Fatal(err)
	}
	var out [][]imap.Flag
	for _, m := range ms {
		out = append(out, m.Flags)
	}
	return out
}

func has(fs []imap.Flag, f imap.Flag) bool {
	for _, x := range fs {
		if strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}

func folder(t *testing.T, fs []provider.Folder, name string) provider.Folder {
	t.Helper()
	for _, f := range fs {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no folder %s in %+v", name, fs)
	return provider.Folder{}
}

var moveCaps = imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapMove: {}, imap.CapUIDPlus: {}}

func TestFoldersAndSkipList(t *testing.T) {
	r := newRig(t, moveCaps, false)
	ctx := context.Background()
	if err := r.a.Check(ctx); err != nil {
		t.Fatal(err)
	}
	fs, err := r.a.Folders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"INBOX": provider.KindInbox, "Sent": provider.KindSent, "Trash": provider.KindSkip,
		"Drafts": provider.KindSkip, "Clients": provider.KindOther}
	for name, kind := range want {
		if f := folder(t, fs, name); f.Kind != kind {
			t.Errorf("%s: kind %s, want %s", name, f.Kind, kind)
		}
	}
}

func TestStartTLSAndBadPassword(t *testing.T) {
	r := newRig(t, moveCaps, true)
	if err := r.a.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.a.c.Password = "wrong"
	if err := r.a.Check(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

// UID resume, PEEK (MOD-EML-08) and a UIDVALIDITY change (MOD-EML-07).
func TestChangesResumePeekAndUIDValidity(t *testing.T) {
	r := newRig(t, moveCaps, false)
	ctx := context.Background()
	d := r.direct()
	for i := 1; i <= 3; i++ {
		r.put(d, "Clients", r.raw(i, "first batch"))
	}
	fs, _ := r.a.Folders(ctx)
	f := folder(t, fs, "Clients")
	refs, cur, err := r.a.Changes(ctx, f, provider.Cursor{}, time.Time{}, 2)
	if err != nil || len(refs) != 2 || cur.LastUID != 2 || cur.UIDValidity == 0 {
		t.Fatalf("first page %v %+v %v", refs, cur, err)
	}
	refs2, cur, err := r.a.Changes(ctx, f, cur, time.Time{}, 2)
	if err != nil || len(refs2) != 1 || cur.LastUID != 3 {
		t.Fatalf("second page %v %+v %v", refs2, cur, err)
	}
	refs3, cur3, err := r.a.Changes(ctx, f, cur, time.Time{}, 2)
	if err != nil || len(refs3) != 0 || cur3.LastUID != 3 {
		t.Fatalf("nothing new %v %+v %v", refs3, cur3, err)
	}
	raw, err := r.a.Fetch(ctx, refs[0])
	if err != nil || !strings.Contains(string(raw), "Body first batch") {
		t.Fatalf("fetch %q %v", raw, err)
	}
	for _, fl := range r.flags(d, "Clients") {
		if has(fl, imap.FlagSeen) {
			t.Fatal("reading set \\Seen")
		}
	}
	// The server rebuilds the folder: a new UIDVALIDITY, and the old refs are gone.
	d.Unselect().Wait()
	if err := d.Delete("Clients").Wait(); err != nil {
		t.Fatal(err)
	}
	d.Create("Clients", nil).Wait()
	for i := 1; i <= 3; i++ {
		r.put(d, "Clients", r.raw(i, "first batch"))
	}
	if _, err := r.a.Fetch(ctx, refs[0]); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("stale ref: want ErrNotFound, got %v", err)
	}
	again, cur4, err := r.a.Changes(ctx, f, cur, time.Time{}, 0)
	if err != nil || len(again) != 3 || cur4.UIDValidity == cur.UIDValidity {
		t.Fatalf("after UIDVALIDITY change %v %+v %v", again, cur4, err)
	}
	// since limits a first read by date.
	since, _, err := r.a.Changes(ctx, f, provider.Cursor{}, time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), 0)
	if err != nil || len(since) != 2 {
		t.Fatalf("since: %v %v", since, err)
	}
}

func testMoveAndDelete(t *testing.T, caps imap.CapSet) {
	r := newRig(t, caps, false)
	ctx := context.Background()
	d := r.direct()
	r.put(d, "INBOX", r.raw(1, "to move"))
	r.put(d, "INBOX", r.raw(2, "to delete"))
	r.put(d, "INBOX", r.raw(3, "to label"))
	fs, _ := r.a.Folders(ctx)
	refs, _, err := r.a.Changes(ctx, folder(t, fs, "INBOX"), provider.Cursor{}, time.Time{}, 0)
	if err != nil || len(refs) != 3 {
		t.Fatal(refs, err)
	}
	nr, err := r.a.Move(ctx, refs[0], provider.ReviewLaterFolder)
	if err != nil || nr.Folder != provider.ReviewLaterFolder {
		t.Fatalf("move %+v %v", nr, err)
	}
	raw, err := r.a.Fetch(ctx, nr)
	if err != nil || !strings.Contains(string(raw), "to move") {
		t.Fatalf("fetch moved %v", err)
	}
	if _, err := r.a.Fetch(ctx, refs[0]); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("old ref after move: %v", err)
	}
	if err := r.a.AfterFile(ctx, refs[1], provider.AfterFileAction{Kind: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Fetch(ctx, refs[1]); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("deleted message still there: %v", err)
	}
	if err := r.a.AfterFile(ctx, refs[2], provider.AfterFileAction{Kind: "label"}); err != nil {
		t.Fatal(err)
	}
	fl := r.flags(d, "INBOX")
	if len(fl) != 1 || !has(fl[0], FiledKeyword) || has(fl[0], imap.FlagSeen) {
		t.Fatalf("inbox flags %v", fl)
	}
	// Court: marked read, then moved to Filed.
	if err := r.a.AfterFile(ctx, nr, provider.AfterFileAction{Kind: "move", MarkRead: true}); err != nil {
		t.Fatal(err)
	}
	fl = r.flags(d, provider.FiledFolder)
	if len(fl) != 1 || !has(fl[0], imap.FlagSeen) {
		t.Fatalf("filed flags %v", fl)
	}
	if err := r.a.MarkRead(ctx, refs[2], true); err != nil {
		t.Fatal(err)
	}
}

func TestMoveWithMOVE(t *testing.T) { testMoveAndDelete(t, moveCaps) }

// A server without MOVE or UIDPLUS gets COPY, STORE \Deleted and EXPUNGE.
func TestMoveFallback(t *testing.T) { testMoveAndDelete(t, imap.CapSet{imap.CapIMAP4rev1: {}}) }

func TestSendAppendsSentCopy(t *testing.T) {
	r := newRig(t, moveCaps, false)
	ctx := context.Background()
	raw := r.raw(4, "outgoing")
	id, err := r.a.Send(ctx, raw, provider.Envelope{From: r.login, To: []string{"pat@client.test", "hidden@client.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(id, "@client.test") {
		t.Fatalf("id %q", id)
	}
	m := r.smtp.Messages()
	if len(m) != 1 || len(m[0].To) != 2 || !m[0].TLS {
		t.Fatalf("smtp got %+v", m)
	}
	d := r.direct()
	if fl := r.flags(d, "Sent"); len(fl) != 1 || !has(fl[0], imap.FlagSeen) {
		t.Fatalf("sent copy flags %v", fl)
	}
	r.a.c.Password = "wrong"
	if _, err := r.a.Send(ctx, raw, provider.Envelope{From: r.login, To: []string{"pat@client.test"}}); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

func TestRefusesPlainSMTP(t *testing.T) {
	r := newRig(t, moveCaps, false)
	scfg, _ := testmail.Cert()
	plain := testmail.NewSMTP(scfg, false, false)
	defer plain.Close()
	r.a.c.SMTPPort = plain.Port
	if _, err := r.a.Send(context.Background(), r.raw(5, "x"), provider.Envelope{To: []string{"pat@client.test"}}); err == nil {
		t.Fatal("sent over a server without STARTTLS")
	}
	if len(plain.Messages()) != 0 {
		t.Fatal("message reached the plain server")
	}
}
