package email_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
)

// MOD-EML-01: the password goes only to the core's secret store.
func TestMODEML01PasswordOnlyInCoreSecretStore(t *testing.T) {
	e := newEnv(t)
	pw := "pw-" + randomHex(10)
	body := map[string]any{"role": "filing", "address": filingAddr, "provider": "imap", "imap_host": "127.0.0.1", "imap_port": 993,
		"smtp_host": "127.0.0.1", "smtp_port": 587, "login": filingAddr, "password": pw}
	code, resp, _ := e.call(http.MethodPost, "/api/accounts", e.admin, "", body)
	if code != http.StatusCreated {
		t.Fatalf("connect: %d %s", code, resp)
	}
	var a account
	json.Unmarshal(resp, &a)
	if strings.Contains(string(resp), pw) {
		t.Fatal("the connect answer holds the password")
	}
	if got := e.core.Secrets["account/"+a.ID+"/password"]; got != pw {
		t.Fatalf("secret store has %q", got)
	}
	// No column of any mod_email table holds it.
	rs, err := e.db.Module.Query(`SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = $1 AND data_type IN ('text', 'ARRAY')`, e.db.Schema)
	if err != nil {
		t.Fatal(err)
	}
	var cols [][2]string
	for rs.Next() {
		var tb, c string
		rs.Scan(&tb, &c)
		cols = append(cols, [2]string{tb, c})
	}
	rs.Close()
	for _, c := range cols {
		if n := e.count(`SELECT count(*) FROM `+c[0]+` WHERE `+c[1]+`::text LIKE '%' || $1 || '%'`, pw); n > 0 {
			t.Fatalf("%s.%s holds the password", c[0], c[1])
		}
	}
	for _, p := range []string{"/api/accounts", "/api/accounts/" + a.ID, "/accounts"} {
		_, b, _ := e.call(http.MethodGet, p, e.admin, "", nil)
		if strings.Contains(string(b), pw) {
			t.Fatalf("%s shows the password", p)
		}
	}
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Hello", "hi").Raw())
	e.sync()
	if strings.Contains(e.logs.String(), pw) {
		t.Fatal("a log line holds the password")
	}
}

// MOD-EML-03: an OAuth callback with an unknown or expired state stores nothing.
func TestMODEML03OAuthCallbackBadState(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{"?state=unknown&code=abc", "?code=abc", "?state=&code="} {
		code, _, _ := e.call(http.MethodGet, "/oauth/callback"+q, nil, "", nil)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, code)
		}
	}
	// An expired state: start a flow, then let it run out.
	e.core.Settings["google_client_id"] = "client-id"
	e.core.Settings["google_client_secret"] = "client-" + randomHex(6)
	var start struct {
		URL string `json:"url"`
	}
	e.json(http.StatusOK, http.MethodPost, "/api/oauth/start", e.attorney, map[string]string{"provider": "gmail", "role": "personal"}, &start)
	i := strings.Index(start.URL, "state=")
	if i < 0 {
		t.Fatalf("no state in %s", start.URL)
	}
	state := strings.SplitN(start.URL[i+6:], "&", 2)[0]
	e.advance(2 * time.Hour)
	code, _, _ := e.call(http.MethodGet, "/oauth/callback?state="+state+"&code=abc", nil, "", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("expired state: %d", code)
	}
	if n := e.count(`SELECT count(*) FROM accounts`); n != 0 || len(e.core.Secrets) != 0 {
		t.Fatalf("stored %d accounts, %d secrets", n, len(e.core.Secrets))
	}
}

// MOD-EML-04: a sign-in failure marks needs_reauth and notifies the right person once.
func TestMODEML04SignInFailureNotifiesOnce(t *testing.T) {
	e := newEnv(t)
	firm := e.connect(e.admin, "filing", filingAddr)
	personal := e.connect(e.attorney, "personal", "jordan@firm.example.test")
	e.box(filingAddr).FailSignIn = true
	e.box("jordan@firm.example.test").FailSignIn = true
	e.sync()
	e.sync()
	e.sync()
	for _, id := range []string{firm.ID, personal.ID} {
		var a account
		e.json(http.StatusOK, http.MethodGet, "/api/accounts/"+id, e.admin, nil, &a)
		if a.Status != "needs_reauth" {
			t.Fatalf("%s status %s", id, a.Status)
		}
	}
	got := map[string]int{}
	for _, n := range e.core.Notifications() {
		if n.Kind == "email.account_problem" {
			got[n.UserID]++
			if !n.Emailed || !strings.HasPrefix(n.Link, "/modules/email/") {
				t.Fatalf("notice %+v", n)
			}
		}
	}
	if got[e.admin.ID] != 1 || got[e.attorney.ID] != 1 || len(got) != 2 {
		t.Fatalf("notices %v", got)
	}
	// It recovers, fails again: one more notice.
	e.box(filingAddr).FailSignIn = false
	e.advance(10 * time.Minute)
	e.sync()
	e.box(filingAddr).FailSignIn = true
	e.advance(10 * time.Minute)
	e.sync()
	got = map[string]int{}
	for _, n := range e.core.Notifications() {
		if n.Kind == "email.account_problem" {
			got[n.UserID]++
		}
	}
	if got[e.admin.ID] != 2 {
		t.Fatalf("after recovery: %v", got)
	}
}

// MOD-EML-05: staff can't reach another user's personal account.
func TestMODEML05PersonalAccountHidden(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.attorney, "personal", "jordan@firm.example.test")
	for _, p := range []string{"/api/accounts/" + a.ID} {
		if code, _, _ := e.call(http.MethodGet, p, e.staff, "", nil); code != http.StatusNotFound {
			t.Fatalf("GET %s as staff: %d", p, code)
		}
	}
	if code, _, _ := e.call(http.MethodPost, "/api/accounts/"+a.ID+"/pause", e.staff, "", map[string]any{}); code != http.StatusNotFound {
		t.Fatalf("pause as staff: %d", code)
	}
	var list struct{ Data []account }
	e.json(http.StatusOK, http.MethodGet, "/api/accounts", e.staff, nil, &list)
	for _, x := range list.Data {
		if x.ID == a.ID {
			t.Fatal("listed to staff")
		}
	}
}

// MOD-EML-06: two concurrent runs over the same 3 messages make 3 rows, one filing each.
func TestMODEML06ConcurrentSync(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.admin, "filing", filingAddr)
	for i := 0; i < 3; i++ {
		e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Question "+string(rune('A'+i)), "about the estate").Raw())
	}
	st, domains, err := settingsAndDomains(e)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := e.m.Store.Account(context.Background(), a.ID)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.m.Syncer.SyncAccount(context.Background(), acct, st, domains)
		}()
	}
	wg.Wait()
	e.sync()
	if n := len(e.rows()); n != 3 {
		t.Fatalf("%d rows", n)
	}
	if f := e.filings(); len(f) != 3 {
		t.Fatalf("%d filings", len(f))
	}
	if n := len(e.core.NotesOn(e.m42.ID)); n != 3 {
		t.Fatalf("%d notes", n)
	}
}

// MOD-EML-07: a changed UIDVALIDITY re-reads the folder without duplicates.
func TestMODEML07UIDValidityChange(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	b := e.box("jordan@firm.example.test")
	for i := 0; i < 3; i++ {
		b.Deliver("INBOX", msg("someone@else.example.test", "jordan@firm.example.test", "Note "+string(rune('A'+i)), "x").Raw())
	}
	e.sync()
	b.ChangeUIDValidity("INBOX")
	e.sync()
	e.sync()
	if n := len(e.rows()); n != 3 {
		t.Fatalf("%d rows after UIDVALIDITY change", n)
	}
}

// MOD-EML-08: reading never marks mail read.
func TestMODEML08ReadingNeverMarksRead(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	b := e.box("jordan@firm.example.test")
	r := b.Deliver("INBOX", msg("someone@else.example.test", "jordan@firm.example.test", "Hello", "x").Raw())
	e.sync()
	rows := e.rows()
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	// Opening it in the module reads it from the store, not the mailbox.
	e.call(http.MethodGet, "/messages/"+rows[0].ID, e.attorney, "", nil)
	e.call(http.MethodGet, "/api/messages/"+rows[0].ID, e.attorney, "", nil)
	e.call(http.MethodGet, "/preview/"+rows[0].ID, e.attorney, "", nil)
	if len(b.ReadChanges) != 0 || b.Seen(r) {
		t.Fatalf("read flag changed: %v", b.ReadChanges)
	}
}

// MOD-EML-09: a message gone between list and fetch is skipped and not retried.
func TestMODEML09GoneBetweenListAndFetch(t *testing.T) {
	e := newEnv(t)
	e.connect(e.attorney, "personal", "jordan@firm.example.test")
	b := e.box("jordan@firm.example.test")
	r := b.Deliver("INBOX", msg("someone@else.example.test", "jordan@firm.example.test", "Vanishing", "x").Raw())
	b.Deliver("INBOX", msg("someone@else.example.test", "jordan@firm.example.test", "Staying", "x").Raw())
	b.DropOnFetch(r)
	if err := e.m.Syncer.Pass(context.Background(), true); err != nil {
		t.Fatalf("pass failed: %v", err)
	}
	e.sync()
	if n := len(e.rows()); n != 1 {
		t.Fatalf("%d rows", n)
	}
	if n := b.Fetches[r.String()]; n != 1 {
		t.Fatalf("fetched the gone message %d times", n)
	}
}

// MOD-EML-10: skipped folders are never read.
func TestMODEML10SkippedFolders(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	b := e.box(filingAddr)
	for _, f := range []string{"Trash", "Junk", "Drafts", provider.FiledFolder, provider.ReviewLaterFolder} {
		b.Deliver(f, msg(patAddr, filingAddr, "In "+f, "x").Raw())
	}
	e.sync()
	if n := len(e.rows()); n != 0 {
		t.Fatalf("%d rows from skipped folders", n)
	}
	for k := range b.Fetches {
		t.Fatalf("fetched %s", k)
	}
}

func settingsAndDomains(e *env) (st config.Settings, domains []string, err error) {
	domains, st, err = e.m.Syncer.Domains(context.Background())
	return st, domains, err
}
