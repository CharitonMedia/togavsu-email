package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/ids"
	"github.com/CharitonMedia/togavsu-email/internal/pgtest"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

func setup(t *testing.T) (*store.Store, *pgtest.DB, string) {
	db := pgtest.New(t)
	admin := ids.Now()
	db.Record(t, "users", admin)
	return store.New(db.Module, nil), db, admin
}

// MOD-EML-02: the database refuses a court account that deletes and a personal account
// with no owner.
func TestAccountChecks(t *testing.T) {
	s, _, admin := setup(t)
	ctx := context.Background()
	court := &store.Account{Provider: "imap", Role: "court", Address: "efile@firm.example.test", CreatedBy: admin, AfterFile: "delete"}
	if err := s.InsertAccount(ctx, court); err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("court delete: %v", err)
	}
	court.AfterFile, court.ID = "label", ""
	if err := s.InsertAccount(ctx, court); err == nil || !strings.Contains(err.Error(), "accounts_court_moves") {
		t.Fatalf("court label: %v", err)
	}
	personal := &store.Account{Provider: "imap", Role: "personal", Address: "me@firm.example.test", CreatedBy: admin, AfterFile: "label"}
	if err := s.InsertAccount(ctx, personal); err == nil || !strings.Contains(err.Error(), "accounts_personal_owner") {
		t.Fatalf("ownerless personal: %v", err)
	}
	court.AfterFile, court.ID = "move", ""
	if err := s.InsertAccount(ctx, court); err != nil {
		t.Fatal(err)
	}
	dup := &store.Account{Provider: "imap", Role: "filing", Address: "EFILE@firm.example.test", CreatedBy: admin, AfterFile: "delete"}
	if err := s.InsertAccount(ctx, dup); err == nil {
		t.Fatal("a second account with the same address (any case) was stored")
	}
	a := &store.Account{Provider: "imap", Role: "filing", Address: "clients@firm.example.test", CreatedBy: admin, AfterFile: "delete", SystemMail: true}
	b := &store.Account{Provider: "imap", Role: "filing", Address: "other@firm.example.test", CreatedBy: admin, AfterFile: "delete", SystemMail: true}
	if err := s.InsertAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAccount(ctx, b); err == nil {
		t.Fatal("two system mail accounts")
	}
}

// The unique keys behind MOD-EML-06 (one row per provider message, however many runs
// claim it) and MOD-EML-20 (one filing per matter and Message-ID).
func TestClaimAndFilingKeys(t *testing.T) {
	s, db, admin := setup(t)
	ctx := context.Background()
	acct := &store.Account{Provider: "imap", Role: "filing", Address: "clients@firm.example.test", CreatedBy: admin, AfterFile: "delete"}
	if err := s.InsertAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	won := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Claim(ctx, &store.Message{AccountID: acct.ID, ProviderRef: "INBOX/1.1", Status: "pending"})
			if err != nil {
				t.Error(err)
			}
			won <- ok
		}()
	}
	wg.Wait()
	close(won)
	n := 0
	for ok := range won {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d runs claimed the same message", n)
	}
	matter, note, file := ids.Now(), ids.Now(), ids.Now()
	db.Record(t, "matters", matter)
	db.Record(t, "notes", note)
	db.Record(t, "files", file)
	m1 := &store.Message{AccountID: acct.ID, ProviderRef: "INBOX/1.2", RFCMessageID: "<x@client.example.test>"}
	m2 := &store.Message{AccountID: acct.ID, ProviderRef: "INBOX/1.3", RFCMessageID: "<x@client.example.test>"}
	for _, m := range []*store.Message{m1, m2} {
		if _, err := s.Claim(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordFiling(ctx, store.Filing{MessageID: m1.ID, MatterID: matter, RFCMessageID: m1.RFCMessageID, NoteID: note, HTMLFileID: file, How: "auto_address"}, "inbound"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFiling(ctx, store.Filing{MessageID: m2.ID, MatterID: matter, RFCMessageID: m2.RFCMessageID, NoteID: note, HTMLFileID: file, How: "auto_address"}, "inbound"); err == nil {
		t.Fatal("the same Message-ID filed twice to one matter")
	}
	// A core matter with a filing can't be deleted (MOD-EML-50's module half).
	if _, err := db.Core.ExecContext(ctx, `DELETE FROM core.matters WHERE id = $1`, matter); err == nil {
		t.Fatal("a matter with a filing was deleted")
	}
}

// MOD-EML-45: a sent system mail's row keeps no body.
func TestSystemMailBodyCleared(t *testing.T) {
	s, db, _ := setup(t)
	ctx := context.Background()
	o := &store.Outbound{Kind: "system", IdempotencyKey: "sys-1", To: []string{"pat@client.example.test"}, Subject: "Reset", BodyText: "reset link", BodySHA256: "abc"}
	if existing, err := s.BeginOutbound(ctx, o); err != nil || existing {
		t.Fatal(existing, err)
	}
	if err := s.FinishOutbound(ctx, o.ID, "sent", "p1", "", true); err != nil {
		t.Fatal(err)
	}
	var text, html, sha string
	if err := db.Module.QueryRowContext(ctx, `SELECT body_text, body_html, body_sha256 FROM outbound WHERE id = $1`, o.ID).Scan(&text, &html, &sha); err != nil {
		t.Fatal(err)
	}
	if text != "" || html != "" || sha != "abc" {
		t.Fatalf("body kept: %q %q %q", text, html, sha)
	}
	again := &store.Outbound{Kind: "system", IdempotencyKey: "sys-1"}
	if existing, err := s.BeginOutbound(ctx, again); err != nil || !existing || again.ProviderRef != "p1" {
		t.Fatalf("replay: %v %v %+v", existing, err, again)
	}
}

func TestUnfiledNoticeGap(t *testing.T) {
	s, db, _ := setup(t)
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	u := ids.Now()
	db.Record(t, "users", u)
	ctx := context.Background()
	for i, want := range []bool{true, false} {
		if ok, err := s.ClaimUnfiledNotice(ctx, u, 15*time.Minute); err != nil || ok != want {
			t.Fatalf("claim %d: %v %v", i, ok, err)
		}
	}
	now = now.Add(16 * time.Minute)
	if ok, _ := s.ClaimUnfiledNotice(ctx, u, 15*time.Minute); !ok {
		t.Fatal("no notice after the gap")
	}
}
