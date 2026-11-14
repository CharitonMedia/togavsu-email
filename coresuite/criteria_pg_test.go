package coresuite_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
	"github.com/CharitonMedia/togavsu/pkg/modulekit/coretest"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/kittest"
	"github.com/CharitonMedia/togavsu-email/internal/render"
	"github.com/CharitonMedia/togavsu-email/internal/store"
	"github.com/CharitonMedia/togavsu-email/internal/webhook"
)

// The signed package installs only from a trusted signer, runs nothing before the
// admin approves it, and then runs on its own login in its own schema, which can't read
// core tables; its tab renders through the core's frame for a signed-in viewer.
func TestCoreInstallsTheSignedPackage(t *testing.T) {
	c := coretest.New(t)
	var mod *email.Module
	c.Serve(email.ID, func(m coretest.Module) http.Handler {
		mod = email.New(email.Config{Core: m.CoreURL, Token: m.Token, WebhookSecret: m.WebhookSecret, DB: m.DB, TempDir: t.TempDir()})
		return mod.Handler()
	})
	files, _ := kittest.Installable(t)
	pub, priv := kittest.Key(t)
	signed, err := modulekit.Build(priv, files)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.TryInstall(signed); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an untrusted signer was accepted: %d %s", r.Code, r.Body)
	}
	_, other := kittest.Key(t)
	c.TrustKey("Email module test key", pub)
	forged, _ := modulekit.Build(other, files)
	if r := c.TryInstall(forged); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a package signed by another key was accepted: %d %s", r.Code, r.Body)
	}
	c.Install(signed)
	if len(c.Runtime.Deployments()) != 0 || c.SchemaExists(email.ID) {
		t.Fatal("the module ran before approval")
	}
	c.ApproveAll(email.ID)
	dep := c.Last(email.ID)
	if dep.Version != "1.0.0" || dep.Secrets["togavsu_api_token"] == "" || dep.Secrets["togavsu_webhook_secret"] == "" || !c.SchemaExists(email.ID) || mod == nil {
		t.Fatalf("deployment %+v", dep)
	}
	ctx := context.Background()
	db := c.ModuleDB(email.ID)
	for _, table := range []string{"accounts", "messages", "filings", "outbound", "matter_mail", "received_events"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM core.contacts LIMIT 1`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the module login read core contacts: %v", err)
	}
	page := c.Tab(coretest.Staff, email.ID, "inbox", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body, "<h1>Inbox</h1>") {
		t.Fatalf("inbox tab: %d %s", page.Code, page.Body)
	}
	if r := c.Tab(coretest.Staff, email.ID, "matter", c.Open); r.Code != http.StatusOK {
		t.Fatalf("matter tab: %d %s", r.Code, r.Body)
	}
}

// MOD-EML-02: on the module's own login, the database refuses a court account that
// deletes and a personal account without an owner.
func TestMODEML02AccountChecks(t *testing.T) {
	k := newCore(t)
	ctx := context.Background()
	court := store.Account{Provider: "imap", Role: "court", Address: courtAddr, CreatedBy: k.Admin.ID, AfterFile: "delete", SyncFrom: 1}
	if err := k.store().InsertAccount(ctx, &court); err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("court delete: %v", err)
	}
	personal := store.Account{Provider: "imap", Role: "personal", Address: "ada@firm.example", CreatedBy: k.Admin.ID, AfterFile: "label", SyncFrom: 1}
	if err := k.store().InsertAccount(ctx, &personal); err == nil || !strings.Contains(err.Error(), "accounts_personal_owner") {
		t.Fatalf("personal without owner: %v", err)
	}
	// The owner must be a real core user.
	personal.ID, personal.OwnerUserID = "", "01a1aaaa-0000-7000-8000-000000000000"
	if err := k.store().InsertAccount(ctx, &personal); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("personal with an unknown owner: %v", err)
	}
	if n := k.count(`SELECT count(*) FROM accounts`); n != 0 {
		t.Fatalf("%d accounts stored", n)
	}
}

// MOD-EML-03: an OAuth callback with an unknown or expired state stores nothing and
// answers 400, through the core's /m/email/ route.
func TestMODEML03OAuthState(t *testing.T) {
	k := newCore(t)
	if r := k.Admin.Do(http.MethodGet, "/m/email/oauth/callback?state=unknown-state&code=abc", nil); r.Code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d %s", r.Code, r.Body)
	}
	err := k.store().InsertState(context.Background(), store.OAuthState{State: "expired-" + randomHex(8), AccountID: "acct", UserID: k.Admin.ID,
		Provider: "gmail", Role: "filing", CodeVerifier: "v", RedirectURI: "https://firm.example/m/email/oauth/callback", ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := k.m.Store.DB.QueryRow(`SELECT state FROM oauth_states`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if r := k.Admin.Do(http.MethodGet, "/m/email/oauth/callback?state="+state+"&code=abc", nil); r.Code != http.StatusBadRequest {
		t.Fatalf("expired state: %d %s", r.Code, r.Body)
	}
	if n := k.count(`SELECT count(*) FROM accounts`); n != 0 {
		t.Fatalf("%d accounts stored", n)
	}
}

// MOD-EML-05: staff asking the accounts API for another user's personal account get not
// found; the owner and the admin see it. Viewers are the core's, by context token.
func TestMODEML05OtherPersonalAccount(t *testing.T) {
	k := newCore(t)
	a := k.account("personal", "ada@firm.example", k.Attorney, nil)
	if r := k.page(coretest.Staff, http.MethodGet, "/api/accounts/"+a.ID, nil); r.Code != http.StatusNotFound {
		t.Fatalf("staff: %d %s", r.Code, r.Body)
	}
	if r := k.page(coretest.Staff, http.MethodGet, "/api/accounts", nil); strings.Contains(r.Body, a.ID) {
		t.Fatalf("staff list shows it: %s", r.Body)
	}
	for _, who := range []string{coretest.Attorney, coretest.Admin} {
		if r := k.page(who, http.MethodGet, "/api/accounts/"+a.ID, nil); r.Code != http.StatusOK || !strings.Contains(r.Body, a.ID) {
			t.Fatalf("%s: %d %s", who, r.Code, r.Body)
		}
	}
	if r := k.user(coretest.Staff).Do(http.MethodGet, "/m/email/api/accounts/"+a.ID+"?context=not-a-token", nil); r.Code == http.StatusOK {
		t.Fatalf("a bad context token was accepted: %s", r.Body)
	}
}

// MOD-EML-19: a queue row whose only candidate is the core's walled matter shows staff
// off the wall no matter name or number; the attorney on the wall sees the suggestion.
func TestMODEML19WalledCandidate(t *testing.T) {
	k := newCore(t)
	a := k.account("filing", filingAddr, nil, nil)
	m := k.message(a, "inbox", "unfiled", "quinn@client.example", "Merger docs", "x", func(m *store.Message) { m.MatchReason = "walled_matter" })
	if err := k.store().SetMatches(context.Background(), m.ID, []store.Match{{MatterID: k.Walled, Via: "address", Rank: 1}}); err != nil {
		t.Fatal(err)
	}
	walled := k.matterOf(coretest.Attorney, k.Walled) // the admin is off coretest's wall
	name, number := str(t, walled, "name"), str(t, walled, "number")
	for _, p := range []string{"/api/queue", "/api/queue/" + m.ID, "/unfiled", "/file/" + m.ID} {
		r := k.page(coretest.Staff, http.MethodGet, p, nil)
		if r.Code >= 500 || strings.Contains(r.Body, name) || strings.Contains(r.Body, number) || strings.Contains(r.Body, k.Walled) {
			t.Fatalf("%s as staff (%d) shows the walled matter: %s", p, r.Code, r.Body)
		}
	}
	r := k.page(coretest.Attorney, http.MethodGet, "/api/queue/"+m.ID, nil)
	if r.Code != http.StatusOK || !strings.Contains(r.Body, number) {
		t.Fatalf("attorney on the wall: %d %s", r.Code, r.Body)
	}
}

// MOD-EML-27: GET /api/v1/matters/{id}?include=email.mail on the core returns the
// module's filed count and last mail time to a viewer on the matter, and not found to a
// viewer off its wall.
func TestMODEML27MatterInclude(t *testing.T) {
	k := newCore(t)
	k.requireFilings()
	a := k.account("filing", filingAddr, nil, nil)
	for _, matter := range []string{k.Walled, k.Walled, k.Open} {
		k.filed(k.message(a, "inbox", "pending", clientAddr, "Filed "+randomHex(2), "x", nil), coretest.Attorney, matter)
	}
	r := k.user(coretest.Attorney).Do(http.MethodGet, "/api/v1/matters/"+k.Walled+"?include=email.mail", nil)
	if r.Code != http.StatusOK || !strings.Contains(r.Body, `"filed_count":2`) || !strings.Contains(r.Body, `"last_mail_at"`) {
		t.Fatalf("attorney: %d %s", r.Code, r.Body)
	}
	if r := k.user(coretest.Staff).Do(http.MethodGet, "/api/v1/matters/"+k.Walled+"?include=email.mail", nil); r.Code != http.StatusNotFound {
		t.Fatalf("staff off the wall: %d %s", r.Code, r.Body)
	}
	if r := k.user(coretest.Staff).Do(http.MethodGet, "/api/v1/matters/"+k.Open+"?include=email.mail", nil); r.Code != http.StatusOK || !strings.Contains(r.Body, `"filed_count":1`) {
		t.Fatalf("staff on the open matter: %d %s", r.Code, r.Body)
	}
}

// MOD-EML-29: dismissing without a reason is refused. Dismissing a court row with a
// reason marks it read at the provider, which opens the mailbox with its sealed password.
func TestMODEML29Dismiss(t *testing.T) {
	k := newCore(t)
	a := k.account("court", courtAddr, nil, nil)
	m := k.message(a, "inbox", "unfiled", "noticing@court.example", "Notice", "x", nil)
	if r := k.page(coretest.Staff, http.MethodPost, "/api/queue/"+m.ID+"/dismiss", map[string]any{}); r.Code != http.StatusBadRequest {
		t.Fatalf("dismiss without a reason: %d %s", r.Code, r.Body)
	}
	if got, _ := k.store().Message(context.Background(), m.ID); got.Status != "unfiled" {
		t.Fatalf("status %s", got.Status)
	}
	t.Skip("needs CORE-06: dismissing a court row with a reason marks it read in the mailbox, opened with its password from the core's secret store")
}

// MOD-EML-31: staff can't see, preview or file another user's personal unfiled row.
func TestMODEML31OtherPersonalRow(t *testing.T) {
	k := newCore(t)
	a := k.account("personal", "ada@firm.example", k.Attorney, nil)
	m := k.message(a, "inbox", "unfiled", "friend@else.example", "Private", "x", nil)
	for _, req := range []struct{ method, path string }{{"GET", "/api/queue/" + m.ID}, {"GET", "/preview/" + m.ID}, {"GET", "/file/" + m.ID},
		{"POST", "/api/queue/" + m.ID + "/file"}} {
		var body any
		if req.method == "POST" {
			body = map[string]any{"matter_id": k.Open}
		}
		if r := k.page(coretest.Staff, req.method, req.path, body); r.Code != http.StatusNotFound {
			t.Fatalf("staff %s %s: %d %s", req.method, req.path, r.Code, r.Body)
		}
	}
	if r := k.page(coretest.Staff, http.MethodGet, "/api/queue", nil); strings.Contains(r.Body, m.ID) {
		t.Fatalf("staff queue lists it: %s", r.Body)
	}
	if r := k.page(coretest.Attorney, http.MethodGet, "/preview/"+m.ID, nil); r.Code != http.StatusOK {
		t.Fatalf("owner preview: %d", r.Code)
	}
	if n := len(k.coreNotes(k.Open)); n != 0 {
		t.Fatalf("%d notes written", n)
	}
}

// MOD-EML-32: the preview carries the module's CSP and no script, and the core frames
// it in its own sandbox.
func TestMODEML32PreviewCSP(t *testing.T) {
	k := newCore(t)
	a := k.account("filing", filingAddr, nil, nil)
	m := k.message(a, "inbox", "unfiled", clientAddr, "With script", "x", func(m *store.Message) {
		m.BodyHTML = `<p>Hello</p><script>alert(1)</script><img src="https://tracker.example/p.gif" onerror="x()">`
	})
	resp, b := k.direct(http.MethodGet, "/preview/"+m.ID+"?context="+k.token(coretest.Staff, ""), nil, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") != render.PreviewCSP {
		t.Fatalf("%d CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if body := strings.ToLower(string(b)); strings.Contains(body, "<script") || strings.Contains(body, "onerror") || strings.Contains(body, "tracker.example") {
		t.Fatalf("preview: %s", b)
	}
	if r := k.page(coretest.Staff, http.MethodGet, "/preview/"+m.ID, nil); r.Code != http.StatusOK || strings.Contains(strings.ToLower(r.Body), "<script") {
		t.Fatalf("through the core: %d %s", r.Code, r.Body)
	}
}

// MOD-EML-33: the core's signed matter.updated event reaches the module, which verifies
// it with the deployment's webhook secret and records it once. Re-matching the waiting
// notice then asks the core's mail matching.
func TestMODEML33EventRematch(t *testing.T) {
	k := newCore(t)
	k.Admin.Must(http.StatusOK, http.MethodPatch, "/api/v1/matters/"+k.Open, map[string]any{"name": "Estate of Harlow (26AB-CV00123)"})
	if sent := k.DeliverEvents(); sent == 0 {
		t.Fatal("no event delivered")
	}
	if n := k.Count(`SELECT count(*) FROM core.module_deliveries WHERE module_id = 'email' AND status <> 'delivered'`); n != 0 {
		t.Fatalf("%d deliveries not delivered", n)
	}
	if n := k.count(`SELECT count(*) FROM received_events WHERE action = 'matter.updated' AND record_id = $1`, k.Open); n != 1 {
		t.Fatalf("module recorded %d matter.updated events; log: %s", n, k.logs)
	}
	if k.DeliverEvents() != 0 {
		t.Fatal("the event was delivered twice")
	}
	t.Skip("needs CORE-26: re-matching the waiting court notice calls matchMail, and filing it takes a CORE-27 filing grant")
}

// MOD-EML-34: quick email to an invalid address is 400 and sends nothing.
func TestMODEML34InvalidAddress(t *testing.T) {
	k := newCore(t)
	k.account("filing", filingAddr, nil, func(a *store.Account) { a.DefaultSend = true })
	r := k.page(coretest.Staff, http.MethodPost, "/api/send", map[string]any{"to": "not an address", "subject": "Hi", "body": "Hello"})
	if r.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if n := len(k.box(filingAddr).SentMessages()); n != 0 || k.count(`SELECT count(*) FROM outbound`) != 0 {
		t.Fatalf("sent %d", n)
	}
}

// MOD-EML-36: quick email naming a matter the viewer is walled off from is not found and
// sends nothing; the core decides the wall.
func TestMODEML36WalledMatter(t *testing.T) {
	k := newCore(t)
	k.account("filing", filingAddr, nil, func(a *store.Account) { a.DefaultSend = true })
	r := k.page(coretest.Staff, http.MethodPost, "/api/send", map[string]any{"to": clientAddr, "subject": "Hi", "body": "Hello", "matter_id": k.Walled})
	if r.Code != http.StatusNotFound || strings.Contains(r.Body, "Quist") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if n := len(k.box(filingAddr).SentMessages()); n != 0 || k.count(`SELECT count(*) FROM outbound`) != 0 {
		t.Fatalf("sent %d", n)
	}
}

func systemBody(id string) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "kind": "password_reset", "to": "sam@firm.example",
		"subject": "Reset your password", "text": "Use the link to reset your password."})
	return b
}

// MOD-EML-43: a system mail with a bad signature is 401 and sends nothing; the module
// checks it against the webhook secret the core handed the deployment.
func TestMODEML43BadSignature(t *testing.T) {
	k := newCore(t)
	k.account("filing", filingAddr, nil, func(a *store.Account) { a.SystemMail = true })
	for _, sig := range []string{"", "t=1,v1=deadbeef", webhook.Sign("not-the-secret", time.Now(), systemBody("x1"))} {
		h := http.Header{"Content-Type": {"application/json"}}
		if sig != "" {
			h.Set("X-Togavsu-Signature", sig)
		}
		if resp, b := k.direct(http.MethodPost, "/system-mail", systemBody("x1"), h); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%q: %d %s", sig, resp.StatusCode, b)
		}
	}
	if n := len(k.box(filingAddr).SentMessages()); n != 0 || k.count(`SELECT count(*) FROM outbound`) != 0 {
		t.Fatalf("sent %d", n)
	}
}

// MOD-EML-44: with no system mail account, a correctly signed system mail answers 503
// not_configured. The core then sending it by SMTP is CC-1.
func TestMODEML44NotConfigured(t *testing.T) {
	k := newCore(t)
	resp, b := k.signedPost("/system-mail", systemBody("y1"), http.Header{"Idempotency-Key": {"y1"}})
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(b), "not_configured") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	t.Skip("needs CORE-05: the core routing system mail to the module and falling back to SMTP on not_configured")
}

// MOD-EML-46: an email search as staff returns no row filed to a matter walled off from
// them (the core decides) and no other user's personal mail.
func TestMODEML46Search(t *testing.T) {
	k := newCore(t)
	k.requireFilings()
	firm := k.account("filing", filingAddr, nil, nil)
	personal := k.account("personal", "ada@firm.example", k.Attorney, nil)
	k.filed(k.message(firm, "inbox", "pending", clientAddr, "Contract open", "contract", nil), coretest.Attorney, k.Open)
	k.filed(k.message(firm, "inbox", "pending", "quinn@client.example", "Contract walled", "contract", nil), coretest.Attorney, k.Walled)
	k.message(personal, "inbox", "inbox", "friend@else.example", "Contract personal", "contract", nil)
	k.message(firm, "inbox", "unfiled", "stranger@else.example", "Contract queued", "contract", nil)
	staff := k.page(coretest.Staff, http.MethodGet, "/api/search?q=contract", nil)
	if staff.Code != http.StatusOK || !strings.Contains(staff.Body, "Contract open") || !strings.Contains(staff.Body, "Contract queued") ||
		strings.Contains(staff.Body, "Contract walled") || strings.Contains(staff.Body, "Contract personal") {
		t.Fatalf("staff: %d %s", staff.Code, staff.Body)
	}
	att := k.page(coretest.Attorney, http.MethodGet, "/api/search?q=contract", nil)
	if !strings.Contains(att.Body, "Contract walled") || !strings.Contains(att.Body, "Contract personal") {
		t.Fatalf("attorney: %s", att.Body)
	}
}

// MOD-EML-48: the core targets the Inbox widget at the inbox tab with its folder and the
// Unfiled widget at the unfiled tab with its account, and the widget frames the core
// draws pass those filters to the module.
func TestMODEML48WidgetTargets(t *testing.T) {
	k := newCore(t)
	m, err := modulekit.ParseManifest(k.files[modulekit.ManifestFile])
	if err != nil {
		t.Fatal(err)
	}
	ws, err := m.DashboardWidgets()
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]string{}
	for _, w := range ws {
		targets[w.ID] = w.Target
	}
	if targets["inbox"] != "/modules/email/tabs/inbox" || targets["unfiled"] != "/modules/email/tabs/unfiled" {
		t.Fatalf("targets %v", targets)
	}
	for _, tab := range []string{"inbox", "unfiled"} {
		if r := k.Tab(coretest.Staff, email.ID, tab, ""); r.Code != http.StatusOK {
			t.Fatalf("%s tab: %d", tab, r.Code)
		}
	}

	filing := k.account("filing", filingAddr, nil, nil)
	court := k.account("court", courtAddr, nil, nil)
	k.message(filing, "inbox", "unfiled", "stranger@else.example", "Filing side", "x", nil)
	k.message(court, "inbox", "unfiled", "noticing@court.example", "Court side", "x", nil)
	own := k.account("personal", "ada@firm.example", k.Attorney, nil)
	k.message(own, "inbox", "inbox", "friend@else.example", "In the inbox", "x", nil)
	k.message(own, "sent", "inbox", "ada@firm.example", "In sent", "x", nil)

	frame := func(who, path string) string {
		t.Helper()
		page := k.user(who).Must(http.StatusOK, http.MethodGet, path, nil)
		i := strings.Index(page.Body, `src="/m/email/`)
		if i < 0 {
			t.Fatalf("no module frame in %s: %s", path, page.Body)
		}
		src := page.Body[i+len(`src="`):]
		src = strings.ReplaceAll(src[:strings.Index(src, `"`)], "&amp;", "&")
		return k.user(who).Must(http.StatusOK, http.MethodGet, src, nil).Body
	}
	if b := frame(coretest.Admin, "/modules/email/widgets/unfiled?account_id="+filing.ID); !strings.Contains(b, "Filing side") || strings.Contains(b, "Court side") {
		t.Fatalf("unfiled widget for the filing account: %s", b)
	}
	if b := frame(coretest.Attorney, "/modules/email/widgets/inbox?folder=sent"); !strings.Contains(b, "In sent") || strings.Contains(b, "In the inbox") {
		t.Fatalf("inbox widget on sent: %s", b)
	}
	if b := frame(coretest.Attorney, "/modules/email/widgets/inbox"); strings.Contains(b, "In sent") || !strings.Contains(b, "In the inbox") {
		t.Fatalf("inbox widget: %s", b)
	}
}

// MOD-EML-49: installed with ai:use and modules:read declined (mail:system is not on the
// pinned core), the module deploys and runs; the core refuses its AI calls, which the
// module reads as ai:use declined; system mail is unavailable. Declining the required
// notes:write cancels the install.
func TestMODEML49DeclinedScopes(t *testing.T) {
	k := newCoreWith(t, "ai:use", "modules:read")
	ctx := context.Background()
	err := k.m.Core.AICall(ctx, k.viewer(coretest.Attorney), "court_notice_pick", map[string]any{"candidates": []string{}}, nil)
	if !coreapi.IsScopeDenied(err) {
		t.Fatalf("AI call with ai:use declined: %v", err)
	}
	var st map[string]struct {
		Available bool `json:"available"`
	}
	resp, b := k.direct(http.MethodGet, "/api/status", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, &st); err != nil || st["system_mail"].Available {
		t.Fatalf("status %s", b)
	}
	if r := k.Tab(coretest.Staff, email.ID, "inbox", ""); r.Code != http.StatusOK {
		t.Fatalf("inbox tab: %d", r.Code)
	}

	// A second core in its own subtest, so it gets a database of its own.
	t.Run("notes write declined", func(t *testing.T) {
		cancelled := install(t)
		if r := cancelled.decide("notes:write"); r.Code == http.StatusOK && len(cancelled.Runtime.Deployments()) != 0 {
			t.Fatalf("declining notes:write deployed the module: %d %s", r.Code, r.Body)
		}
		if len(cancelled.Runtime.Deployments()) != 0 || cancelled.SchemaExists(email.ID) {
			t.Fatal("declining notes:write left a deployment or a schema")
		}
		if r := cancelled.Admin.Do(http.MethodGet, "/api/v1/modules/email", nil); r.Code == http.StatusOK && strings.Contains(r.Body, `"enabled":true`) {
			t.Fatalf("module still installed: %s", r.Body)
		}
	})

	t.Skip("needs CORE-28 and CORE-26: the status reads modules:read from the device-context-tokens refusal (CORE-28), and court AI pick learns ai:use from its first call in mail matching (CORE-26)")
}

// MOD-EML-50: the module's foreign keys refuse a hard delete of a core matter the module
// points at, and the core's matter merge moves filings, message_matches and matter_mail
// rows to the survivor in its own transaction. matter_mail rows come only from filings,
// so that half waits on notesGap.
func TestMODEML50DeleteAndMerge(t *testing.T) {
	k := newCore(t)
	a := k.account("filing", filingAddr, nil, nil)
	gone, _ := k.Matter("Estate of Harlow (duplicate)", "", nil)
	filings := k.notesDurable()
	if filings {
		k.filed(k.message(a, "inbox", "pending", clientAddr, "To merge", "x", nil), coretest.Attorney, gone)
	}
	q := k.message(a, "inbox", "unfiled", clientAddr, "Candidate", "x", nil)
	if err := k.store().SetMatches(context.Background(), q.ID, []store.Match{{MatterID: gone, Via: "address", Rank: 1}}); err != nil {
		t.Fatal(err)
	}

	core, err := sql.Open("pgx", k.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	if _, err := core.Exec(`DELETE FROM core.matters WHERE id = $1`, gone); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("hard delete: %v", err)
	}

	r := k.Admin.Do(http.MethodPost, "/api/v1/matters/"+gone+"/merge", map[string]any{"survivor_id": k.Open})
	if r.Code != http.StatusOK || !strings.Contains(r.Body, `"module_id":"email"`) {
		t.Fatalf("merge: %d %s", r.Code, r.Body)
	}
	checks := []struct{ q, matter string }{{`SELECT count(*) FROM message_matches WHERE matter_id = $1`, k.Open}}
	if filings {
		checks = append(checks,
			struct{ q, matter string }{`SELECT count(*) FROM filings WHERE matter_id = $1`, k.Open},
			struct{ q, matter string }{`SELECT count(*) FROM matter_mail WHERE matter_id = $1`, k.Open})
	}
	for _, c := range checks {
		if n := k.count(c.q, c.matter); n != 1 {
			t.Fatalf("%s: %d", c.q, n)
		}
	}
	for _, tbl := range []string{"filings", "message_matches", "matter_mail"} {
		if n := k.count(`SELECT count(*) FROM `+tbl+` WHERE matter_id = $1`, gone); n != 0 {
			t.Fatalf("%d %s rows left on the merged matter", n, tbl)
		}
	}
	if !filings {
		t.Skip(notesGap + "; the filings and matter_mail half of the merge is untested")
	}
	t.Skip("the pinned core has no matter hard-delete operation, so no core refusal names the module; no CORE item covers it yet")
}

// MOD-EML-51: uninstall with keep leaves mod_email in place and a reinstall reads its
// accounts and filings; uninstall with delete drops the schema after the backup.
func TestMODEML51UninstallAndReinstall(t *testing.T) {
	k := newCore(t)
	a := k.account("filing", filingAddr, nil, nil)
	filings := k.notesDurable()
	if filings {
		k.filed(k.message(a, "inbox", "pending", clientAddr, "Kept", "x", nil), coretest.Attorney, k.Open)
	}

	k.Uninstall(email.ID, false)
	if !k.SchemaExists(email.ID) {
		t.Fatal("keep dropped the schema")
	}
	if r := k.user(coretest.Staff).Do(http.MethodGet, "/modules/email/tabs/inbox", nil); r.Code != http.StatusNotFound {
		t.Fatalf("tab outlived uninstall: %d", r.Code)
	}
	old := k.m
	k.Install(k.signed())
	k.ApproveAll(email.ID)
	if k.m == old {
		t.Fatal("no new deployment")
	}
	r := k.page(coretest.Admin, http.MethodGet, "/api/accounts/"+a.ID, nil)
	if r.Code != http.StatusOK || !strings.Contains(r.Body, filingAddr) {
		t.Fatalf("reinstalled account: %d %s", r.Code, r.Body)
	}
	if filings {
		if n := k.count(`SELECT count(*) FROM filings`); n != 1 {
			t.Fatalf("%d filings after reinstall", n)
		}
		if r := k.user(coretest.Attorney).Do(http.MethodGet, "/api/v1/matters/"+k.Open+"?include=email.mail", nil); !strings.Contains(r.Body, `"filed_count":1`) {
			t.Fatalf("include after reinstall: %s", r.Body)
		}
	}

	k.Uninstall(email.ID, true)
	if k.SchemaExists(email.ID) || len(k.Backups.Made()) != 1 {
		t.Fatalf("delete left the schema or took %d backups", len(k.Backups.Made()))
	}
	if !filings {
		t.Skip(notesGap + "; filings surviving a reinstall are untested")
	}
	t.Skip("needs CORE-06: uninstall with delete also drops the module's sealed secrets")
}
