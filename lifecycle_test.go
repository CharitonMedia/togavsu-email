package email_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
)

// MOD-EML-49, module side: with mail:system, ai:use and modules:read declined the
// module keeps working and reports the three features unavailable. The install flow
// itself (and declining notes:write cancelling it) is the core's: CORE-05, CORE-28.
func TestMODEML49DeclinedOptionalScopes(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{"mail:system", "ai:use", "modules:read"} {
		e.core.Scopes[s] = false
	}
	e.connect(e.admin, "filing", filingAddr)
	e.connect(e.admin, "court", courtAddr)
	chris := e.core.AddContact("Chris", "Rivera", "chris@client.example.test")
	m2 := e.core.AddMatter("2026-0070", "Rivera purchase", "open", "")
	e.core.AddParty(m2, chris, "client")
	e.box(courtAddr).Deliver("INBOX", e.courtNotice("Notice of filing", "Rivera v. Acme Holdings. A document was filed.").Raw())
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Still files", "x").Raw())
	e.sync()
	if fs := e.filings(); len(fs) != 1 || fs[0].MatterID != e.m42.ID {
		t.Fatalf("filings %+v", fs)
	}
	if n := len(e.only("unfiled")); n != 1 {
		t.Fatalf("court notice: rows %+v", e.rows())
	}
	var st map[string]struct {
		Available bool `json:"available"`
	}
	e.json(http.StatusOK, http.MethodGet, "/api/status", nil, nil, &st)
	for _, k := range []string{"system_mail", "court_ai_pick", "outlook_addin"} {
		if st[k].Available {
			t.Fatalf("%s reported available: %+v", k, st)
		}
	}
	code, _ := e.addin(http.MethodGet, "/addin/api/ping", e.core.NewDevice(e.attorney), nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("add-in with modules:read declined: %d", code)
	}
	code, b := e.signed("/system-mail", systemBody("z1"), http.Header{"Idempotency-Key": {"z1"}})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("system mail: %d %s", code, b)
	}
}

// MOD-EML-50, module side: the module's foreign keys refuse a hard delete of a core
// matter that has a filing. Naming the module in the refusal and moving rows on a
// merge are the core's (CORE-29 harness; core merge).
func TestMODEML50MatterDeleteRefused(t *testing.T) {
	e := newEnv(t)
	e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Filed", "x").Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "")
	_, err := e.db.Core.Exec(`DELETE FROM core.matters WHERE id = $1`, e.m42.ID)
	if err == nil || !strings.Contains(err.Error(), "violates foreign key constraint") {
		t.Fatalf("delete: %v", err)
	}
}

// MOD-EML-51, module side: a reinstall over a kept schema picks up its accounts and
// filings and reads on without duplicates. Locking the schema, dropping it and its
// sealed secrets on uninstall with delete are the core's (CORE-06).
func TestMODEML51ReinstallKeepsData(t *testing.T) {
	e := newEnv(t)
	a := e.connect(e.admin, "filing", filingAddr)
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "Before", "x").Raw())
	e.sync()
	e.filedTo(t, e.m42.ID, "")
	// A new deployment of the module over the same schema and secrets.
	e.m = email.New(email.Config{Core: e.core.URL(), Token: e.core.Token, WebhookSecret: e.hook, DB: e.db.Module,
		Now: e.now, Log: slog.New(slog.NewTextHandler(e.logs, nil)), Factory: e.boxes.Factory(), TempDir: t.TempDir()})
	e.h = e.m.Handler()
	var got account
	e.json(http.StatusOK, http.MethodGet, "/api/accounts/"+a.ID, e.admin, nil, &got)
	if got.Status != "connected" {
		t.Fatalf("account %+v", got)
	}
	e.box(filingAddr).Deliver("INBOX", msg(patAddr, filingAddr, "After", "x").Raw())
	e.sync()
	if fs := e.filings(); len(fs) != 2 {
		t.Fatalf("filings %+v", fs)
	}
	if n := len(e.rows()); n != 2 {
		t.Fatalf("rows %d", n)
	}
}

// The failure cases the acceptance plan names for filing grants: a matter outside the
// match token is forbidden and a walled matter is not found.
func TestFilingGrantRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	other := e.core.AddMatter("2026-0099", "Someone else", "open", "")
	ans, err := e.m.Core.MatchMail(ctx, coreapi.Service, coreapi.MatchRequest{Addresses: []string{patAddr}})
	if err != nil || ans.MatchToken == "" {
		t.Fatal(ans, err)
	}
	if _, err := e.m.Core.FilingGrant(ctx, ans.MatchToken, other.ID, "k1"); coreapi.StatusOf(err) != http.StatusForbidden {
		t.Fatalf("outside the token: %v", err)
	}
	e.core.Wall(e.m42, e.attorney)
	if _, err := e.m.Core.FilingGrant(ctx, ans.MatchToken, e.m42.ID, "k2"); !coreapi.IsNotFound(err) {
		t.Fatalf("walled: %v", err)
	}
}
