// Package server is the module's HTTP side: the frames the core shows (tabs, widgets,
// panels, compose, queue, preview, search, badge), the JSON API they and the Outlook
// add-in call, the OAuth callback, and the core's signed event and system mail
// webhooks.
//
// Every interactive request carries the context token the core handed the frame (the
// ?context= query parameter, a form field of the same name, or the
// X-Togavsu-Context-Token header from the add-in's own fetches), and every core read is
// made with it, so the core answers exactly what that viewer may see.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/filing"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/store"
	"github.com/CharitonMedia/togavsu-email/internal/syncer"
)

// ModulePrefix is where the core mounts the module (/m/<module id>).
const ModulePrefix = "/m/email"

// Server serves the module.
type Server struct {
	Store         *store.Store
	Core          *coreapi.Client
	Filer         *filing.Filer
	Sender        *send.Sender
	Syncer        *syncer.Syncer
	Open          filing.Mailbox
	Settings      func(ctx context.Context) (config.Settings, error)
	WebhookSecret string
	Now           func() time.Time
	Log           *slog.Logger
	HTTP          *http.Client
	// OAuthToken overrides the providers' token URL, and APIBase their API base by
	// provider (tests).
	OAuthToken string
	APIBase    map[string]string
	// CheckAccount runs a new account's first sign-in check.
	CheckAccount func(ctx context.Context, a store.Account) error

	// aiDenied is set once the core refused ai:use.
	aiDenied atomic.Bool
}

// AIDenied records the core's answer to the last AI call.
func (s *Server) AIDenied(denied bool) { s.aiDenied.Store(denied) }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler routes every module path.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	// Frames.
	m.HandleFunc("GET /inbox", s.page(s.inboxPage))
	m.HandleFunc("GET /unfiled", s.page(s.unfiledPage))
	m.HandleFunc("GET /accounts", s.page(s.accountsPage))
	m.HandleFunc("GET /matter", s.page(s.matterPage))
	m.HandleFunc("GET /matter/recent", s.page(s.recentPage))
	m.HandleFunc("GET /compose", s.page(s.composePage))
	m.HandleFunc("GET /widget/inbox", s.page(s.inboxWidget))
	m.HandleFunc("GET /widget/unfiled", s.page(s.unfiledWidget))
	m.HandleFunc("GET /search", s.page(s.searchPage))
	m.HandleFunc("GET /messages/{id}", s.page(s.messagePage))
	m.HandleFunc("GET /file/{id}", s.page(s.fileDialog))
	m.HandleFunc("GET /preview/{id}", s.page(s.preview))
	m.HandleFunc("GET /badge/unfiled", s.api(s.badge))
	m.HandleFunc("GET /static/app.js", appJS)

	// The JSON API the frames' forms and scripts call.
	m.HandleFunc("GET /api/queue", s.api(s.queueList))
	m.HandleFunc("GET /api/queue/{id}", s.api(s.queueRow))
	m.HandleFunc("POST /api/queue/{id}/{action}", s.api(s.queueAction))
	m.HandleFunc("POST /api/queue/defer-all", s.api(s.deferAll))
	m.HandleFunc("GET /api/matters", s.api(s.matterSearch))
	m.HandleFunc("GET /api/matters/{id}/mail", s.api(s.matterMail))
	m.HandleFunc("GET /api/inbox", s.api(s.inboxAPI))
	m.HandleFunc("GET /api/messages/{id}", s.api(s.messageAPI))
	m.HandleFunc("POST /api/messages/{id}/read", s.api(s.markRead))
	m.HandleFunc("POST /api/send", s.api(s.sendAPI))
	m.HandleFunc("GET /api/search", s.api(s.searchAPI))
	m.HandleFunc("GET /api/accounts", s.api(s.listAccounts))
	m.HandleFunc("POST /api/accounts", s.api(s.connectIMAP))
	m.HandleFunc("GET /api/accounts/{id}", s.api(s.getAccount))
	m.HandleFunc("POST /api/accounts/{id}/{action}", s.api(s.accountAction))
	m.HandleFunc("POST /api/oauth/start", s.api(s.oauthStart))
	m.HandleFunc("GET /api/prefs", s.api(s.getPrefs))
	m.HandleFunc("POST /api/prefs", s.api(s.savePrefs))
	m.HandleFunc("GET /api/status", s.statusAPI)
	m.HandleFunc("GET /api/openapi.json", openAPI)

	// No context token: the provider's redirect, the core's webhooks, health.
	m.HandleFunc("GET /oauth/callback", s.oauthCallback)
	m.HandleFunc("POST /events", s.events)
	m.HandleFunc("POST /system-mail", s.systemMail)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	// The Outlook add-in.
	m.HandleFunc("GET /outlook-addin/manifest.xml", s.page(s.addinManifest))
	m.HandleFunc("GET /outlook-addin/{file}", addinFile)
	m.HandleFunc("GET /addin/api/ping", s.addin(s.addinPing))
	m.HandleFunc("POST /addin/api/suggest", s.addin(s.addinSuggest))
	m.HandleFunc("GET /addin/api/matters", s.addin(s.addinMatters))
	m.HandleFunc("GET /addin/api/contacts", s.addin(s.addinContacts))
	m.HandleFunc("POST /addin/api/file", s.addin(s.addinFile))
	m.HandleFunc("GET /addin/api/plus-address", s.addin(s.addinPlus))
	return m
}

// Viewer is the person behind a request.
type Viewer struct {
	coreapi.ViewerInfo
	Token  string
	Matter string // the context token's matter, if any
}

// Auth is the viewer's core auth.
func (v Viewer) Auth() coreapi.Auth { return coreapi.Viewer(v.Token) }

func (v Viewer) send() send.Viewer { return send.Viewer{ViewerInfo: v.ViewerInfo, Token: v.Token} }

// Admin reports the admin role.
func (v Viewer) Admin() bool { return v.Has("admin") && !v.Has("client") }

// Lawyer reports an attorney or admin, who may file attorney-only.
func (v Viewer) Lawyer() bool { return v.Has("admin", "attorney") && !v.Has("client") }

var errNoContext = errors.New("open this page from Togavsu")

func contextToken(r *http.Request) string {
	if t := r.URL.Query().Get("context"); t != "" {
		return t
	}
	if t := r.Header.Get(coreapi.ContextHeader); t != "" {
		return t
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.FormValue("context")
	}
	return ""
}

func (s *Server) viewer(r *http.Request) (Viewer, error) {
	tok := contextToken(r)
	if tok == "" {
		return Viewer{}, errNoContext
	}
	vi, err := s.Core.VerifyContext(r.Context(), tok)
	if err != nil {
		return Viewer{}, errNoContext
	}
	v := Viewer{ViewerInfo: vi, Token: tok}
	if vi.MatterID != nil {
		v.Matter = *vi.MatterID
	}
	return v, nil
}

type handler func(w http.ResponseWriter, r *http.Request, v Viewer) error

// Status errors.
type httpError struct {
	status int
	code   string
	detail string
}

func (e *httpError) Error() string { return e.code + ": " + e.detail }

func fail(status int, code, detail string) error { return &httpError{status, code, detail} }

var (
	errNotFound  = fail(http.StatusNotFound, "not_found", "not found")
	errForbidden = fail(http.StatusForbidden, "forbidden", "not allowed")
)

// statusOf maps an error to its HTTP status and problem code.
func statusOf(err error) (int, string, string) {
	var he *httpError
	var inv *send.Invalid
	switch {
	case errors.As(err, &he):
		return he.status, he.code, he.detail
	case errors.Is(err, errNoContext):
		return http.StatusUnauthorized, "unauthenticated", err.Error()
	case errors.As(err, &inv):
		return http.StatusBadRequest, "validation_failed", inv.Error()
	case errors.Is(err, send.ErrNotFound), errors.Is(err, store.ErrNotFound), coreapi.IsNotFound(err):
		return http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, send.ErrNoAccount):
		return http.StatusConflict, "no_account", "there is no account you may send from"
	case errors.Is(err, send.ErrFailed):
		return http.StatusBadGateway, "send_failed", err.Error()
	case coreapi.IsScopeDenied(err):
		return http.StatusServiceUnavailable, "unavailable", "the module was not granted what this needs"
	case coreapi.StatusOf(err) == http.StatusForbidden:
		return http.StatusForbidden, "forbidden", "not allowed"
	}
	return http.StatusInternalServerError, "internal", "something went wrong"
}

func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	status, code, detail := statusOf(err)
	if status >= http.StatusInternalServerError {
		s.log().Error("email request", "path", r.URL.Path, "err", err)
	}
	writeJSON(w, status, map[string]any{"type": "about:blank", "title": code, "status": status, "code": code, "detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// api wraps a JSON endpoint.
func (s *Server) api(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.viewer(r)
		if err == nil {
			err = h(w, r, v)
		}
		if err != nil {
			s.writeErr(w, r, err)
		}
	}
}

// page wraps an HTML frame page.
func (s *Server) page(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := s.viewer(r)
		if err == nil {
			err = h(w, r, v)
		}
		if err != nil {
			status, _, detail := statusOf(err)
			if status >= http.StatusInternalServerError {
				s.log().Error("email page", "path", r.URL.Path, "err", err)
			}
			writePage(w, status, "Email", messageBody(detail))
		}
	}
}

// isForm reports a browser form post, answered with a redirect back to the page.
func isForm(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/x-www-form-urlencoded") || strings.HasPrefix(ct, "multipart/form-data")
}

// done answers an action: JSON for scripts, a redirect to back for forms.
func done(w http.ResponseWriter, r *http.Request, v Viewer, back string, status int, body any) error {
	if isForm(r) {
		if b := r.FormValue("back"); b != "" && strings.HasPrefix(b, "/") && !strings.HasPrefix(b, "//") {
			back = b
		}
		http.Redirect(w, r, withContext(back, v.Token), http.StatusSeeOther)
		return nil
	}
	writeJSON(w, status, body)
	return nil
}

// withContext adds the context token to a module path, under the core's mount.
func withContext(path, tok string) string {
	u, err := url.Parse(path)
	if err != nil {
		return ModulePrefix + "/"
	}
	q := u.Query()
	q.Set("context", tok)
	u.RawQuery = q.Encode()
	if !strings.HasPrefix(u.Path, ModulePrefix) {
		u.Path = ModulePrefix + u.Path
	}
	return u.String()
}

// decode reads a JSON body or form fields into dst's fields named by json tags.
func decode(r *http.Request, dst any) error {
	if isForm(r) {
		if err := r.ParseForm(); err != nil {
			return fail(http.StatusBadRequest, "validation_failed", "bad form")
		}
		obj := map[string]any{}
		for k, vs := range r.PostForm {
			if k == "context" || k == "back" {
				continue
			}
			switch {
			case strings.HasSuffix(k, "[]"):
				obj[strings.TrimSuffix(k, "[]")] = vs
			case len(vs) == one && (vs[zero] == "on" || vs[zero] == "true" || vs[zero] == "false"):
				obj[k] = vs[0] != "false"
			default:
				obj[k] = vs[0]
			}
		}
		raw, _ := json.Marshal(obj)
		return lenient(raw, dst)
	}
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 30<<20))
	if err := dec.Decode(dst); err != nil && err.Error() != "EOF" {
		return fail(http.StatusBadRequest, "validation_failed", "bad JSON body")
	}
	return nil
}

// lenient decodes form-made JSON where numbers and lists arrive as strings.
func lenient(raw []byte, dst any) error {
	if err := json.Unmarshal(raw, dst); err == nil {
		return nil
	}
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	for k, v := range obj {
		if s, ok := v.(string); ok {
			var n json.Number
			if json.Unmarshal([]byte(s), &n) == nil {
				obj[k] = n
			}
		}
	}
	raw, _ = json.Marshal(obj)
	if err := json.Unmarshal(raw, dst); err != nil {
		return fail(http.StatusBadRequest, "validation_failed", "bad form fields")
	}
	return nil
}

// statusAPI reports what is available, for the accounts tab and the admin.
func (s *Server) statusAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.Settings(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "core_unreachable"})
		return
	}
	sys, err := s.Store.SystemMailAccount(ctx)
	sysOK := err == nil && sys.Status == "connected"
	// modules:read: an exchange of no grant answers 401 when the scope is granted, and
	// scope_denied when it was declined.
	_, err = s.Core.ExchangeDevice(ctx, "", "")
	addin := !coreapi.IsScopeDenied(err)
	writeJSON(w, http.StatusOK, map[string]any{
		"system_mail":   map[string]any{"available": sysOK, "reason": unless(sysOK, "no connected account is marked for system mail, or mail:system was declined")},
		"court_ai_pick": map[string]any{"available": st.CourtAIPick && !s.aiDenied.Load(), "reason": unless(st.CourtAIPick && !s.aiDenied.Load(), "turned off, or ai:use was declined")},
		"outlook_addin": map[string]any{"available": addin, "reason": unless(addin, "modules:read was declined")},
	})
}

func unless(ok bool, reason string) string {
	if ok {
		return ""
	}
	return reason
}
