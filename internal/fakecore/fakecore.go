// Package fakecore is a stand-in core for the module's tests. It serves the parts of the
// current core contract the module calls plus the CORE items it depends on, exactly as
// docs/modules/CORE-CHANGES.md names them: service calls (CORE-01), createNotification
// (CORE-03), the module secret store (CORE-06), origin on publication (CORE-16), mail
// matching (CORE-26), the mail filing grant (CORE-27) and device grants (CORE-28).
//
// Records live in memory; every record id is also written to the stand-in core schema
// of the test database, so the module's foreign keys to core records hold. Walls,
// roles and attorney-only files are applied here the way the core's service layer
// applies them, so a test sees what a real viewer would.
package fakecore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/ids"
	pkgfiles "github.com/CharitonMedia/togavsu-email/package"
)

// Module is the module id the fake serves.
const Module = "email"

// User is a Togavsu user.
type User struct {
	ID     string
	Name   string
	Roles  []string
	Active bool
}

func (u *User) has(roles ...string) bool {
	for _, h := range u.Roles {
		for _, w := range roles {
			if h == w {
				return true
			}
		}
	}
	return false
}

// Matter is a core matter. Wall, when non-nil, lists the only users who may see it.
type Matter struct {
	ID, Number, Name, Status, CaseNumber, PracticeArea string
	Wall                                               []string
}

// Contact is a core contact.
type Contact struct {
	ID, Given, Family string
	Archived          bool
	Emails            []string
}

// Party is a matter party.
type Party struct {
	ID, MatterID, ContactID, Role string
	Primary                       bool
}

// Version is a file version.
type Version struct {
	ID, MediaType, SHA256 string
	Data                  []byte
}

// File is a core file.
type File struct {
	ID, Name, MatterID, Origin, CreatedBy string
	AttorneyOnly                          bool
	Versions                              []*Version
}

// Link is a file link.
type Link struct{ From, To, Relationship, CreatedBy string }

// Note is a core note.
type Note struct {
	ID, MatterID, Title, Body, CreatedBy string
	Tags                                 []string
}

// Notification is one created notification.
type Notification struct {
	UserID, Kind, Title, Link string
	Emailed                   bool
}

type upload struct {
	id, name, matterID, mediaType, sha, owner, status string
	size                                              int64
	data                                              []byte
}

type ctxToken struct {
	userID, matterID string
}

type device struct {
	userID  string
	revoked bool
}

type grant struct {
	matterID, messageKey string
	matterIDs            map[string]bool // match tokens
	match                bool
}

type saved struct {
	status  int
	body    []byte
	bodySum string
}

// Core is the fake.
type Core struct {
	mu sync.Mutex
	// Insert writes a record id to the stand-in core schema.
	Insert func(ctx context.Context, table, id string) error
	// IncludeRead reads the module's extension row for include=email.mail.
	IncludeRead func(ctx context.Context, matterID string) (map[string]any, error)

	Token    string
	Users    map[string]*User
	Matters  map[string]*Matter
	Contacts map[string]*Contact
	Parties  []*Party
	Files    map[string]*File
	Links    []Link
	Notes    map[string]*Note
	Notices  []Notification
	Secrets  map[string]string
	Settings map[string]string
	// Scopes the admin granted; a missing one answers scope_denied.
	Scopes map[string]bool
	// AI answers court_notice_pick.
	AI      func(input map[string]any) map[string]any
	AICalls int
	// FailPublishAt makes the Nth publishFile call (1-based, counted from when it is
	// set) answer 500 once.
	FailPublishAt int
	publishes     int
	// FailNotes makes the next n createNote calls answer 500.
	FailNotes int
	// ChunkSize is the upload chunk size the fake hands out.
	ChunkSize int64
	// Log is every call as "METHOD /path actor".
	Log []string

	uploads map[string]*upload
	tokens  map[string]ctxToken
	devices map[string]*device
	grants  map[string]*grant
	idem    map[string]saved
	Server  *httptest.Server
}

// New returns a fake core with every scope the manifest asks for granted.
func New() *Core {
	c := &Core{Token: "fake-module-token-" + ids.Secret(moduleTokenBytes), Users: map[string]*User{}, Matters: map[string]*Matter{}, Contacts: map[string]*Contact{},
		Files: map[string]*File{}, Notes: map[string]*Note{}, Secrets: map[string]string{}, Settings: map[string]string{}, Scopes: map[string]bool{},
		uploads: map[string]*upload{}, tokens: map[string]ctxToken{}, devices: map[string]*device{}, grants: map[string]*grant{}, idem: map[string]saved{},
		ChunkSize: 1 << 20}
	for _, s := range []string{"matters:read", "contacts:read", "notes:write", "files:read", "files:write", "events:read", "notifications:write",
		"module_secrets:write", "ai:use", "modules:read", "mail:system"} {
		c.Scopes[s] = true
	}
	c.Server = httptest.NewServer(c.handler())
	return c
}

// Close stops the server.
func (c *Core) Close() { c.Server.Close() }

// URL is the fake's base URL.
func (c *Core) URL() string { return c.Server.URL }

func (c *Core) insert(table, id string) {
	if c.Insert != nil {
		if err := c.Insert(context.Background(), table, id); err != nil {
			panic(err)
		}
	}
}

// AddUser adds a user.
func (c *Core) AddUser(name string, roles ...string) *User {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := &User{ID: ids.Now(), Name: name, Roles: roles, Active: true}
	c.Users[u.ID] = u
	c.insert("users", u.ID)
	return u
}

// AddContact adds a contact with addresses.
func (c *Core) AddContact(given, family string, emails ...string) *Contact {
	c.mu.Lock()
	defer c.mu.Unlock()
	ct := &Contact{ID: ids.Now(), Given: given, Family: family, Emails: emails}
	c.Contacts[ct.ID] = ct
	c.insert("contacts", ct.ID)
	return ct
}

// AddMatter adds a matter with its client parties.
func (c *Core) AddMatter(number, name, status, caseNumber string, clients ...*Contact) *Matter {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := &Matter{ID: ids.Now(), Number: number, Name: name, Status: status, CaseNumber: caseNumber, PracticeArea: "General"}
	c.Matters[m.ID] = m
	c.insert("matters", m.ID)
	for i, ct := range clients {
		c.Parties = append(c.Parties, &Party{ID: ids.Now(), MatterID: m.ID, ContactID: ct.ID, Role: "client", Primary: i == zero})
	}
	return m
}

// AddParty adds a non-client party.
func (c *Core) AddParty(m *Matter, ct *Contact, role string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Parties = append(c.Parties, &Party{ID: ids.Now(), MatterID: m.ID, ContactID: ct.ID, Role: role})
}

// Wall limits a matter to users.
func (c *Core) Wall(m *Matter, users ...*User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m.Wall = []string{}
	for _, u := range users {
		m.Wall = append(m.Wall, u.ID)
	}
}

// SetCaseNumber changes a matter's case number.
func (c *Core) SetCaseNumber(m *Matter, cn string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m.CaseNumber = cn
}

// AddFile adds a matter file with one version.
func (c *Core) AddFile(m *Matter, name, mediaType string, data []byte, by *User) *File {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := &File{ID: ids.Now(), Name: name, MatterID: m.ID, Origin: "firm", CreatedBy: by.ID}
	f.Versions = []*Version{{ID: ids.Now(), MediaType: mediaType, SHA256: sum(data), Data: data}}
	c.Files[f.ID] = f
	c.insert("files", f.ID)
	return f
}

// ContextFor mints a context token for user, optionally on a matter.
func (c *Core) ContextFor(u *User, matterID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := "ctx-" + ids.Secret(tokenBytes)
	c.tokens[t] = ctxToken{userID: u.ID, matterID: matterID}
	return t
}

// NewDevice creates an Outlook add-in device grant for user and returns its secret.
func (c *Core) NewDevice(u *User) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := "dev-" + ids.Secret(tokenBytes)
	c.devices[s] = &device{userID: u.ID}
	return s
}

// RevokeDevice revokes a device grant.
func (c *Core) RevokeDevice(secret string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.devices[secret]; ok {
		d.revoked = true
	}
}

// NotesOn lists a matter's notes.
func (c *Core) NotesOn(matterID string) []*Note {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Note
	for _, n := range c.Notes {
		if n.MatterID == matterID {
			out = append(out, n)
		}
	}
	return out
}

// FilesOn lists a matter's files.
func (c *Core) FilesOn(matterID string) []*File {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*File
	for _, f := range c.Files {
		if f.MatterID == matterID {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// LinksFrom lists links from a file.
func (c *Core) LinksFrom(fileID string) []Link {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Link
	for _, l := range c.Links {
		if l.From == fileID {
			out = append(out, l)
		}
	}
	return out
}

// Notifications returns what was sent.
func (c *Core) Notifications() []Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Notification(nil), c.Notices...)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// --- HTTP ---

type actor struct {
	user    *User
	matter  string // context token matter claim
	service bool
	grant   *grant
	label   string
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": code, "status": status, "code": code, "detail": detail, "request_id": "fake"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type opFunc func(w http.ResponseWriter, r *http.Request, a actor)

// op wraps a handler: the module bearer, the scope, who the call runs as, and replay of
// an Idempotency-Key.
func (c *Core) op(scopes []string, modes string, f opFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+c.Token {
			problem(w, http.StatusUnauthorized, "unauthenticated", "bad module token")
			return
		}
		for _, s := range scopes {
			if !c.Scopes[s] {
				problem(w, http.StatusForbidden, "scope_denied", "the module was not granted "+s)
				return
			}
		}
		var a actor
		switch {
		case r.Header.Get("X-Togavsu-Context-Token") != "":
			t, ok := c.tokens[r.Header.Get("X-Togavsu-Context-Token")]
			u := c.Users[t.userID]
			if !ok || u == nil || !u.Active {
				problem(w, http.StatusUnauthorized, "unauthenticated", "bad context token")
				return
			}
			if !strings.Contains(modes, "viewer") {
				problem(w, http.StatusForbidden, "scope_denied", "no viewer form")
				return
			}
			a = actor{user: u, matter: t.matterID, label: u.ID}
		case r.Header.Get("X-Togavsu-Task-Authorization") != "":
			g, ok := c.grants[r.Header.Get("X-Togavsu-Task-Authorization")]
			if !ok || !strings.Contains(modes, "task") {
				problem(w, http.StatusForbidden, "forbidden", "bad task authorization")
				return
			}
			a = actor{grant: g, label: "module:" + Module}
		default:
			if !strings.Contains(modes, "service") {
				problem(w, http.StatusForbidden, "scope_denied", "no service principal form")
				return
			}
			a = actor{service: true, label: "module:" + Module}
		}
		c.Log = append(c.Log, r.Method+" "+r.URL.Path+" "+a.label)
		key := r.Header.Get("Idempotency-Key")
		if key == "" || r.Method == http.MethodGet {
			f(w, r, a)
			return
		}
		// The core scopes a key to the caller and route, and refuses the same key with a
		// different body (internal/modules/http.go, write).
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		ik := a.label + " " + r.Method + " " + r.URL.Path + " " + key
		if s, ok := c.idem[ik]; ok {
			if s.bodySum != sum(raw) {
				problem(w, http.StatusConflict, "idempotency_conflict", "this Idempotency-Key was used with a different body")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replay", "true")
			w.WriteHeader(s.status)
			_, _ = w.Write(s.body)
			return
		}
		rec := httptest.NewRecorder()
		f(rec, r, a)
		if rec.Code < http.StatusInternalServerError {
			c.idem[ik] = saved{rec.Code, rec.Body.Bytes(), sum(raw)}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}
}

// canRead applies roles and walls for a matter.
func (c *Core) canRead(a actor, m *Matter) bool {
	if m == nil {
		return false
	}
	if a.grant != nil {
		return a.grant.matterID == m.ID || a.grant.matterIDs[m.ID]
	}
	if a.service {
		return m.Wall == nil
	}
	if a.user == nil || !a.user.has("admin", "attorney", "staff") {
		return false
	}
	if m.Wall != nil {
		for _, id := range m.Wall {
			if id == a.user.ID {
				return true
			}
		}
		return false
	}
	return true
}

func (c *Core) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/v1/modules/email/context-tokens/verify", c.op(nil, "viewer", c.verify))
	m.HandleFunc("GET /api/v1/modules/email/settings", c.op(nil, "service", c.settings))
	m.HandleFunc("PUT /api/v1/modules/email/secrets/{name...}", c.op([]string{"module_secrets:write"}, "service", c.putSecret))
	m.HandleFunc("GET /api/v1/modules/email/secrets/{name...}", c.op([]string{"module_secrets:write"}, "service", c.getSecret))
	m.HandleFunc("DELETE /api/v1/modules/email/secrets/{name...}", c.op([]string{"module_secrets:write"}, "service", c.deleteSecret))
	m.HandleFunc("POST /api/v1/mail/matches", c.op([]string{"matters:read", "contacts:read"}, "viewer service", c.matchMail))
	m.HandleFunc("POST /api/v1/mail/filing-grants", c.op([]string{"notes:write", "files:write"}, "service", c.filingGrant))
	m.HandleFunc("GET /api/v1/matters", c.op([]string{"matters:read"}, "viewer", c.listMatters))
	m.HandleFunc("GET /api/v1/matters/{id}", c.op([]string{"matters:read"}, "viewer", c.getMatter))
	m.HandleFunc("GET /api/v1/matter-parties", c.op([]string{"matters:read"}, "viewer", c.listParties))
	m.HandleFunc("GET /api/v1/contacts", c.op([]string{"contacts:read"}, "viewer", c.listContacts))
	m.HandleFunc("GET /api/v1/contacts/{id}", c.op([]string{"contacts:read"}, "viewer", c.getContact))
	m.HandleFunc("GET /api/v1/contact-emails", c.op([]string{"contacts:read"}, "viewer", c.contactEmails))
	m.HandleFunc("POST /api/v1/file-uploads", c.op([]string{"files:write"}, "viewer task", c.createUpload))
	m.HandleFunc("PUT /api/v1/file-uploads/{id}", c.op([]string{"files:write"}, "viewer task", c.putChunk))
	m.HandleFunc("POST /api/v1/file-uploads/{id}/complete", c.op([]string{"files:write"}, "viewer task", c.completeUpload))
	m.HandleFunc("POST /api/v1/files", c.op([]string{"files:write"}, "viewer task", c.publish))
	m.HandleFunc("POST /api/v1/files/{id}/links", c.op([]string{"files:write"}, "viewer task", c.link))
	m.HandleFunc("GET /api/v1/files", c.op([]string{"files:read"}, "viewer", c.listFiles))
	m.HandleFunc("GET /api/v1/files/{id}", c.op([]string{"files:read"}, "viewer", c.getFile))
	m.HandleFunc("GET /api/v1/files/{id}/versions", c.op([]string{"files:read"}, "viewer", c.versions))
	m.HandleFunc("GET /api/v1/files/{id}/versions/{vid}/download", c.op([]string{"files:read"}, "viewer", c.download))
	m.HandleFunc("POST /api/v1/notes", c.op([]string{"notes:write"}, "viewer task", c.createNote))
	m.HandleFunc("POST /api/v1/notifications", c.op([]string{"notifications:write"}, "viewer service", c.notify))
	m.HandleFunc("POST /api/v1/ai/calls", c.op([]string{"ai:use"}, "viewer task", c.aiCall))
	m.HandleFunc("POST /api/v1/modules/email/device-context-tokens", c.op([]string{"modules:read"}, "service", c.exchangeDevice))
	return m
}

func (c *Core) verify(w http.ResponseWriter, _ *http.Request, a actor) {
	var matter any
	if a.matter != "" {
		matter = a.matter
	}
	writeJSON(w, http.StatusOK, map[string]any{"viewer_user_id": a.user.ID, "roles": a.user.Roles, "module": Module, "matter_id": matter})
}

func (c *Core) settings(w http.ResponseWriter, _ *http.Request, _ actor) {
	keys := []string{"google_client_id", "google_client_secret", "microsoft_client_id", "microsoft_client_secret", "microsoft_tenant", "firm_domains",
		"poll_minutes", "backfill_days", "case_number_patterns", "court_ai_pick", "summary_recipients", "summary_interval_minutes", "sending_disabled"}
	defaults, err := pkgfiles.Defaults()
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var out []map[string]any
	for _, k := range keys {
		v, ok := c.Settings[k]
		if !ok {
			v = defaults[k]
		}
		out = append(out, map[string]any{"key": k, "value": v})
	}
	writeJSON(w, http.StatusOK, out)
}

var secretName = regexp.MustCompile(`^[a-z0-9/_-]{1,128}$`)

func (c *Core) putSecret(w http.ResponseWriter, r *http.Request, _ actor) {
	name := r.PathValue("name")
	var body struct {
		Value string `json:"value"`
	}
	if !secretName.MatchString(name) || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Value) > 16<<10 {
		problem(w, http.StatusBadRequest, "validation_failed", "bad secret")
		return
	}
	if _, ok := c.Secrets[name]; !ok && len(c.Secrets) >= maxSecrets {
		problem(w, http.StatusBadRequest, "validation_failed", "too many secrets")
		return
	}
	c.Secrets[name] = body.Value
	w.WriteHeader(http.StatusNoContent)
}

func (c *Core) getSecret(w http.ResponseWriter, r *http.Request, _ actor) {
	v, ok := c.Secrets[r.PathValue("name")]
	if !ok {
		problem(w, http.StatusNotFound, "not_found", "secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": r.PathValue("name"), "value": v})
}

func (c *Core) deleteSecret(w http.ResponseWriter, r *http.Request, _ actor) {
	if _, ok := c.Secrets[r.PathValue("name")]; !ok {
		problem(w, http.StatusNotFound, "not_found", "secret")
		return
	}
	delete(c.Secrets, r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

func lowerSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range in {
		out[strings.ToLower(strings.TrimSpace(s))] = true
	}
	return out
}

// inName reports cn written in a matter name with a boundary that excludes a following
// hyphen, so a base number never matches a suffixed one.
func inName(name, cn string) bool {
	n, k := strings.ToUpper(name), strings.ToUpper(cn)
	for i := 0; ; {
		j := strings.Index(n[i:], k)
		if j < zero {
			return false
		}
		s := i + j
		e := s + len(k)
		before := s == zero || !alnum(n[s-1])
		after := e == len(n) || (!alnum(n[e]) && n[e] != '-')
		if before && after {
			return true
		}
		i = s + 1
	}
}

func alnum(b byte) bool { return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }

func (c *Core) clientsOf(matterID string) []map[string]string {
	var out []map[string]string
	for _, p := range c.Parties {
		if p.MatterID == matterID && p.Role == "client" {
			ct := c.Contacts[p.ContactID]
			out = append(out, map[string]string{"contact_id": ct.ID, "given_name": ct.Given, "family_name": ct.Family})
		}
	}
	return out
}

func (c *Core) matchMail(w http.ResponseWriter, r *http.Request, a actor) {
	var req struct {
		Addresses      []string `json:"addresses"`
		MatterNumbers  []string `json:"matter_numbers"`
		CaseNumbers    []string `json:"case_numbers"`
		NameTokens     []string `json:"name_tokens"`
		KnownMatterIDs []string `json:"known_matter_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	for _, l := range [][]string{req.Addresses, req.MatterNumbers, req.CaseNumbers, req.NameTokens, req.KnownMatterIDs} {
		if len(l) > maxMatchEntries {
			problem(w, http.StatusBadRequest, "validation_failed", "at most 50 entries")
			return
		}
	}
	byAddr := map[string][]map[string]string{}
	contactIDs := map[string]bool{}
	for addr := range lowerSet(req.Addresses) {
		byAddr[addr] = []map[string]string{}
		for _, ct := range c.Contacts {
			if ct.Archived {
				continue
			}
			for _, e := range ct.Emails {
				if strings.EqualFold(e, addr) {
					byAddr[addr] = append(byAddr[addr], map[string]string{"contact_id": ct.ID, "given_name": ct.Given, "family_name": ct.Family})
					contactIDs[ct.ID] = true
				}
			}
		}
	}
	numbers, names, known := lowerSet(req.MatterNumbers), lowerSet(req.NameTokens), lowerSet(req.KnownMatterIDs)
	type hit struct {
		m   *Matter
		via []string
	}
	var hits []hit
	ids := make([]string, 0, len(c.Matters))
	for id := range c.Matters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := c.Matters[id]
		if !c.canRead(actor{user: a.user, service: a.service}, m) {
			continue
		}
		var via []string
		for _, p := range c.Parties {
			if p.MatterID == m.ID && contactIDs[p.ContactID] {
				via = append(via, "address")
				break
			}
		}
		if numbers[strings.ToLower(m.Number)] {
			via = append(via, "matter_number")
		}
		for _, cn := range req.CaseNumbers {
			if m.CaseNumber != "" && m.CaseNumber == cn {
				via = append(via, "case_number")
			} else if inName(m.Name, cn) {
				via = append(via, "case_number_in_name")
			}
		}
		for _, cl := range c.clientsOf(m.ID) {
			if names[strings.ToLower(cl["family_name"])] {
				via = append(via, "client_name")
				break
			}
		}
		if known[strings.ToLower(m.ID)] {
			via = append(via, "known")
		}
		if len(via) > 0 && len(hits) < maxMatchEntries {
			hits = append(hits, hit{m, via})
		}
	}
	out := map[string]any{"contacts_by_address": byAddr}
	var ms []map[string]any
	matterIDs := map[string]bool{}
	for _, h := range hits {
		ms = append(ms, map[string]any{"id": h.m.ID, "number": h.m.Number, "name": h.m.Name, "status": h.m.Status, "case_number": h.m.CaseNumber,
			"practice_area": h.m.PracticeArea, "clients": c.clientsOf(h.m.ID), "via": h.via})
		matterIDs[h.m.ID] = true
	}
	if ms == nil {
		ms = []map[string]any{}
	}
	out["matters"] = ms
	if a.service {
		tok := "match-" + ids2()
		c.grants[tok] = &grant{matterIDs: matterIDs, match: true}
		out["match_token"] = tok
	}
	writeJSON(w, http.StatusOK, out)
}

func ids2() string { return ids.Secret(tokenBytes) }

func (c *Core) filingGrant(w http.ResponseWriter, r *http.Request, _ actor) {
	var req struct {
		MatchToken string `json:"match_token"`
		MatterID   string `json:"matter_id"`
		MessageKey string `json:"message_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MessageKey == "" {
		problem(w, http.StatusBadRequest, "validation_failed", "match_token, matter_id and message_key")
		return
	}
	mt, ok := c.grants[req.MatchToken]
	if !ok || !mt.match {
		problem(w, http.StatusForbidden, "forbidden", "bad match token")
		return
	}
	m := c.Matters[req.MatterID]
	if m == nil || m.Wall != nil {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	if !mt.matterIDs[req.MatterID] {
		problem(w, http.StatusForbidden, "forbidden", "the matter is not in the match token")
		return
	}
	tok := "grant-" + ids2()
	c.grants[tok] = &grant{matterID: req.MatterID, messageKey: req.MessageKey}
	writeJSON(w, http.StatusCreated, map[string]any{"authorization": tok, "expires_at": time.Now().Add(filingGrantTTL).UTC().Format(time.RFC3339)})
}

func (c *Core) matterJSON(m *Matter) map[string]any {
	return map[string]any{"id": m.ID, "number": m.Number, "name": m.Name, "status": m.Status, "case_number": m.CaseNumber}
}

func (c *Core) listMatters(w http.ResponseWriter, r *http.Request, a actor) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	var out []map[string]any
	ids := make([]string, 0, len(c.Matters))
	for id := range c.Matters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := c.Matters[id]
		if !c.canRead(a, m) {
			continue
		}
		hay := strings.ToLower(m.Number + " " + m.Name)
		for _, cl := range c.clientsOf(m.ID) {
			hay += " " + strings.ToLower(cl["given_name"]+" "+cl["family_name"])
		}
		if q == "" || strings.Contains(hay, q) {
			out = append(out, c.matterJSON(m))
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "page": map[string]any{"limit": pageLimit, "next_cursor": nil}})
}

func (c *Core) getMatter(w http.ResponseWriter, r *http.Request, a actor) {
	m := c.Matters[r.PathValue("id")]
	if !c.canRead(a, m) {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	out := c.matterJSON(m)
	for _, inc := range r.URL.Query()["include"] {
		if inc == "email.mail" && c.IncludeRead != nil {
			row, err := c.IncludeRead(r.Context(), m.ID)
			if err != nil {
				problem(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			rows := []map[string]any{}
			if row != nil {
				rows = append(rows, row)
			}
			out["included"] = map[string]any{"email.mail": rows}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *Core) listParties(w http.ResponseWriter, r *http.Request, a actor) {
	m := c.Matters[r.URL.Query().Get("matter_id")]
	if !c.canRead(a, m) {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	var out []map[string]any
	for _, p := range c.Parties {
		if p.MatterID == m.ID {
			out = append(out, map[string]any{"id": p.ID, "matter_id": p.MatterID, "contact_id": p.ContactID, "role": p.Role, "primary": p.Primary})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "page": map[string]any{"limit": pageLimit, "next_cursor": nil}})
}

func (c *Core) staff(a actor) bool { return a.user != nil && a.user.has("admin", "attorney", "staff") }

func contactJSON(ct *Contact) map[string]any {
	return map[string]any{"id": ct.ID, "full_name": strings.TrimSpace(ct.Given + " " + ct.Family), "given_name": ct.Given, "family_name": ct.Family, "archived": ct.Archived}
}

func (c *Core) listContacts(w http.ResponseWriter, r *http.Request, a actor) {
	if !c.staff(a) {
		problem(w, http.StatusNotFound, "not_found", "contacts")
		return
	}
	q := strings.ToLower(r.URL.Query().Get("q"))
	var out []map[string]any
	for _, ct := range c.Contacts {
		if strings.Contains(strings.ToLower(ct.Given+" "+ct.Family+" "+strings.Join(ct.Emails, " ")), q) {
			out = append(out, contactJSON(ct))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "page": map[string]any{"limit": contactPageLimit, "next_cursor": nil}})
}

func (c *Core) getContact(w http.ResponseWriter, r *http.Request, a actor) {
	ct := c.Contacts[r.PathValue("id")]
	if ct == nil || !c.staff(a) {
		problem(w, http.StatusNotFound, "not_found", "contact")
		return
	}
	writeJSON(w, http.StatusOK, contactJSON(ct))
}

func (c *Core) contactEmails(w http.ResponseWriter, r *http.Request, a actor) {
	ct := c.Contacts[r.URL.Query().Get("contact_id")]
	if ct == nil || !c.staff(a) {
		problem(w, http.StatusNotFound, "not_found", "contact")
		return
	}
	var out []map[string]any
	for i, e := range ct.Emails {
		out = append(out, map[string]any{"contact_id": ct.ID, "email": e, "primary": i == zero, "effective_to": nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "page": map[string]any{"limit": pageLimit, "next_cursor": nil}})
}

func (c *Core) writeMatter(a actor, matterID string) bool {
	m := c.Matters[matterID]
	if a.grant != nil {
		return a.grant.matterID == matterID && m != nil && m.Wall == nil
	}
	return c.canRead(a, m)
}

func (c *Core) createUpload(w http.ResponseWriter, r *http.Request, a actor) {
	var req struct {
		FileName  string `json:"file_name"`
		MatterID  string `json:"matter_id"`
		MediaType string `json:"media_type"`
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FileName == "" {
		problem(w, http.StatusBadRequest, "validation_failed", "file_name and size_bytes")
		return
	}
	if !c.writeMatter(a, req.MatterID) {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	u := &upload{id: ids.Now(), name: req.FileName, matterID: req.MatterID, mediaType: req.MediaType, sha: req.SHA256, size: req.SizeBytes, owner: a.label, status: "receiving"}
	c.uploads[u.id] = u
	writeJSON(w, http.StatusCreated, c.uploadJSON(u))
}

func (c *Core) uploadJSON(u *upload) map[string]any {
	return map[string]any{"id": u.id, "owner": u.owner, "chunk_size_bytes": c.ChunkSize, "next_offset": len(u.data), "size_bytes": u.size,
		"expires_at": time.Now().Add(uploadTTL).UTC().Format(time.RFC3339), "status": u.status}
}

func (c *Core) putChunk(w http.ResponseWriter, r *http.Request, a actor) {
	u := c.uploads[r.PathValue("id")]
	if u == nil || u.owner != a.label {
		problem(w, http.StatusNotFound, "not_found", "upload")
		return
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start != int64(len(u.data)) {
		problem(w, http.StatusBadRequest, "validation_failed", "Content-Range must continue at "+strconv.Itoa(len(u.data)))
		return
	}
	b, _ := io.ReadAll(r.Body)
	u.data = append(u.data, b...)
	writeJSON(w, http.StatusOK, c.uploadJSON(u))
}

func (c *Core) completeUpload(w http.ResponseWriter, r *http.Request, a actor) {
	u := c.uploads[r.PathValue("id")]
	if u == nil || u.owner != a.label {
		problem(w, http.StatusNotFound, "not_found", "upload")
		return
	}
	if int64(len(u.data)) != u.size || (u.sha != "" && u.sha != sum(u.data)) {
		problem(w, http.StatusBadRequest, "validation_failed", "size or sha256 doesn't match")
		return
	}
	u.status = "staged"
	writeJSON(w, http.StatusOK, c.uploadJSON(u))
}

func (c *Core) fileJSON(f *File) map[string]any {
	var cur any
	if len(f.Versions) > 0 {
		cur = f.Versions[len(f.Versions)-1].ID
	}
	return map[string]any{"id": f.ID, "name": f.Name, "matter_id": f.MatterID, "origin": f.Origin, "attorney_only": f.AttorneyOnly,
		"current_version_id": cur, "created_by": f.CreatedBy}
}

func (c *Core) publish(w http.ResponseWriter, r *http.Request, a actor) {
	if c.FailPublishAt > zero {
		c.publishes++
		if c.publishes == c.FailPublishAt {
			c.FailPublishAt = zero
			problem(w, http.StatusInternalServerError, "internal", "fake: publish interrupted")
			return
		}
	}
	var req struct {
		UploadID     string `json:"upload_id"`
		MatterID     string `json:"matter_id"`
		Name         string `json:"name"`
		AttorneyOnly bool   `json:"attorney_only"`
		Origin       string `json:"origin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	u := c.uploads[req.UploadID]
	if u == nil || u.owner != a.label || u.status != "staged" {
		problem(w, http.StatusNotFound, "not_found", "upload")
		return
	}
	if req.MatterID == "" {
		req.MatterID = u.matterID
	}
	if !c.writeMatter(a, req.MatterID) {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	if req.Origin != "" && req.Origin != "firm" && req.Origin != "received" {
		problem(w, http.StatusBadRequest, "validation_failed", "origin is firm or received")
		return
	}
	if req.Origin == "" {
		req.Origin = "received"
	}
	if req.AttorneyOnly && a.user != nil && !a.user.has("admin", "attorney") {
		problem(w, http.StatusForbidden, "forbidden", "attorney-only needs an attorney")
		return
	}
	name := req.Name
	if name == "" {
		name = u.name
	}
	by := a.label
	f := &File{ID: ids.Now(), Name: name, MatterID: req.MatterID, Origin: req.Origin, CreatedBy: by, AttorneyOnly: req.AttorneyOnly,
		Versions: []*Version{{ID: ids.Now(), MediaType: u.mediaType, SHA256: sum(u.data), Data: u.data}}}
	u.status = "published"
	c.Files[f.ID] = f
	c.insert("files", f.ID)
	writeJSON(w, http.StatusCreated, c.fileJSON(f))
}

func (c *Core) readFile(a actor, id string) *File {
	f := c.Files[id]
	if f == nil {
		return nil
	}
	if a.grant != nil {
		if a.grant.matterID != f.MatterID {
			return nil
		}
		return f
	}
	if !c.canRead(a, c.Matters[f.MatterID]) {
		return nil
	}
	if f.AttorneyOnly && (a.user == nil || !a.user.has("admin", "attorney")) {
		return nil
	}
	return f
}

func (c *Core) link(w http.ResponseWriter, r *http.Request, a actor) {
	var req struct {
		ToFileID     string `json:"to_file_id"`
		Relationship string `json:"relationship"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	from, to := c.readFile(a, r.PathValue("id")), c.readFile(a, req.ToFileID)
	if from == nil || to == nil {
		problem(w, http.StatusNotFound, "not_found", "file")
		return
	}
	if req.Relationship != "attachment" {
		problem(w, http.StatusBadRequest, "validation_failed", "relationship")
		return
	}
	l := Link{From: from.ID, To: to.ID, Relationship: req.Relationship, CreatedBy: a.label}
	c.Links = append(c.Links, l)
	writeJSON(w, http.StatusCreated, map[string]any{"id": ids.Now(), "from_file_id": l.From, "to_file_id": l.To, "relationship": l.Relationship})
}

func (c *Core) listFiles(w http.ResponseWriter, r *http.Request, a actor) {
	var out []map[string]any
	for _, f := range c.Files {
		if f.MatterID == r.URL.Query().Get("matter_id") && c.readFile(a, f.ID) != nil {
			out = append(out, c.fileJSON(f))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "page": map[string]any{"limit": filePageLimit, "next_cursor": nil}})
}

func (c *Core) getFile(w http.ResponseWriter, r *http.Request, a actor) {
	f := c.readFile(a, r.PathValue("id"))
	if f == nil {
		problem(w, http.StatusNotFound, "not_found", "file")
		return
	}
	writeJSON(w, http.StatusOK, c.fileJSON(f))
}

func (c *Core) versions(w http.ResponseWriter, r *http.Request, a actor) {
	f := c.readFile(a, r.PathValue("id"))
	if f == nil {
		problem(w, http.StatusNotFound, "not_found", "file")
		return
	}
	var out []map[string]any
	for _, v := range f.Versions {
		out = append(out, map[string]any{"id": v.ID, "file_id": f.ID, "size_bytes": len(v.Data), "sha256": v.SHA256, "media_type": v.MediaType})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "page": map[string]any{"limit": pageLimit, "next_cursor": nil}})
}

func (c *Core) download(w http.ResponseWriter, r *http.Request, a actor) {
	f := c.readFile(a, r.PathValue("id"))
	if f == nil {
		problem(w, http.StatusNotFound, "not_found", "file")
		return
	}
	for _, v := range f.Versions {
		if v.ID == r.PathValue("vid") {
			w.Header().Set("Content-Type", v.MediaType)
			_, _ = io.Copy(w, bytes.NewReader(v.Data))
			return
		}
	}
	problem(w, http.StatusNotFound, "not_found", "version")
}

func (c *Core) createNote(w http.ResponseWriter, r *http.Request, a actor) {
	var req struct {
		MatterID string   `json:"matter_id"`
		Title    string   `json:"title"`
		Body     string   `json:"body"`
		Tags     []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" || req.Body == "" {
		problem(w, http.StatusBadRequest, "validation_failed", "title and body")
		return
	}
	if !c.writeMatter(a, req.MatterID) {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	if c.FailNotes > zero {
		c.FailNotes--
		problem(w, http.StatusInternalServerError, "internal", "fake: note write failed")
		return
	}
	n := &Note{ID: ids.Now(), MatterID: req.MatterID, Title: req.Title, Body: req.Body, Tags: req.Tags, CreatedBy: a.label}
	c.Notes[n.ID] = n
	c.insert("notes", n.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": n.ID, "matter_id": n.MatterID, "title": n.Title})
}

// notificationDefaults are the manifest kinds' default channels (CORE-03).
var notificationDefaults = map[string]bool{"account_problem": true, "unfiled_mail": false}

func (c *Core) notify(w http.ResponseWriter, r *http.Request, _ actor) {
	var req struct {
		UserID string `json:"user_id"`
		Kind   string `json:"kind"`
		Title  string `json:"title"`
		Link   string `json:"link"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	emailed, ok := notificationDefaults[req.Kind]
	u := c.Users[req.UserID]
	if !ok || u == nil || !u.Active || req.Title == "" || !strings.HasPrefix(req.Link, "/modules/"+Module+"/") {
		problem(w, http.StatusBadRequest, "validation_failed", "kind, user, title or link")
		return
	}
	c.Notices = append(c.Notices, Notification{UserID: req.UserID, Kind: Module + "." + req.Kind, Title: req.Title, Link: req.Link, Emailed: emailed})
	writeJSON(w, http.StatusCreated, map[string]any{"created": one, "notification_ids": []string{ids.Now()}})
}

func (c *Core) aiCall(w http.ResponseWriter, r *http.Request, a actor) {
	var req struct {
		Task  string         `json:"task"`
		Input map[string]any `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Task != "court_notice_pick" {
		problem(w, http.StatusBadRequest, "validation_failed", "task")
		return
	}
	if a.grant != nil && !a.grant.match {
		problem(w, http.StatusForbidden, "forbidden", "only a match token runs mail_matching")
		return
	}
	c.AICalls++
	out := map[string]any{"matter_id": ""}
	if c.AI != nil {
		out = c.AI(req.Input)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": ids.Now(), "task": req.Task, "provider": "fake", "model": "fake", "output": out})
}

func (c *Core) exchangeDevice(w http.ResponseWriter, r *http.Request, _ actor) {
	var req struct {
		DeviceSecret string `json:"device_secret"`
		MatterID     string `json:"matter_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	d, ok := c.devices[req.DeviceSecret]
	if !ok || d.revoked || c.Users[d.userID] == nil || !c.Users[d.userID].Active {
		problem(w, http.StatusUnauthorized, "unauthenticated", "the device grant is revoked or unknown")
		return
	}
	u := c.Users[d.userID]
	if req.MatterID != "" && !c.canRead(actor{user: u}, c.Matters[req.MatterID]) {
		problem(w, http.StatusNotFound, "not_found", "matter")
		return
	}
	t := "ctx-" + ids.Secret(tokenBytes)
	c.tokens[t] = ctxToken{userID: u.ID, matterID: req.MatterID}
	writeJSON(w, http.StatusCreated, map[string]any{"token": t, "expires_at": time.Now().Add(deviceTokenTTL).UTC().Format(time.RFC3339),
		"viewer_user_id": u.ID, "roles": u.Roles, "matter_id": nil, "allowed_operations": []string{}})
}
