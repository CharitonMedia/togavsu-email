package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/mailbox"
	"github.com/CharitonMedia/togavsu-email/internal/oauth"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// mayManage: the admin manages every account; anyone on staff manages their own
// personal account. An accountant or client manages none.
func mayManage(v Viewer, a store.Account) bool {
	if v.Admin() {
		return true
	}
	return v.Staff() && a.Role == "personal" && a.OwnerUserID == v.UserID
}

func (s *Server) account(ctx context.Context, v Viewer, id string) (store.Account, error) {
	a, err := s.Store.Account(ctx, id)
	if err != nil || a.Status == "removed" || !mayManage(v, a) {
		return store.Account{}, errNotFound
	}
	return a, nil
}

func (s *Server) visibleAccounts(ctx context.Context, v Viewer) ([]store.Account, error) {
	if !v.Staff() {
		return nil, errNotFound
	}
	all, err := s.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := []store.Account{}
	for _, a := range all {
		if a.Status != "removed" && mayManage(v, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request, v Viewer) error {
	as, err := s.visibleAccounts(r.Context(), v)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": as})
	return nil
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request, v Viewer) error {
	a, err := s.account(r.Context(), v, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, a)
	return nil
}

// roleDefaults applies section 8.1: personal labels after filing and sends; filing
// deletes after a clean filing and takes plus addresses; court moves to Filed and
// never sends.
func roleDefaults(a *store.Account) {
	switch a.Role {
	case "personal":
		// Gmail adds the Togavsu/Filed label, Graph the category of that name, IMAP
		// the Togavsu-Filed keyword; the message stays in its folder.
		a.AfterFile, a.CanSend = "label", true
	case "filing":
		a.AfterFile, a.CanSend, a.PlusAddressing = "delete", true, true
	case "court":
		a.AfterFile, a.CanSend = "move", false
	}
}

type connectBody struct {
	Role         string `json:"role"`
	Address      string `json:"address"`
	Label        string `json:"label"`
	OwnerUserID  string `json:"owner_user_id"`
	Provider     string `json:"provider"`
	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPSecurity string `json:"imap_security"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPSecurity string `json:"smtp_security"`
	Login        string `json:"login"`
	Password     string `json:"password"`
}

// newAccount checks who may connect what: the admin connects filing, court and any
// personal account; anyone on staff connects their own personal account.
func newAccount(v Viewer, b connectBody, prov string) (store.Account, error) {
	if !v.Staff() {
		return store.Account{}, errNotFound
	}
	switch b.Role {
	case "personal":
		if b.OwnerUserID == "" {
			b.OwnerUserID = v.UserID
		}
		if b.OwnerUserID != v.UserID && !v.Admin() {
			return store.Account{}, errForbidden
		}
	case "filing", "court":
		if !v.Admin() {
			return store.Account{}, errForbidden
		}
		b.OwnerUserID = ""
	default:
		return store.Account{}, fail(http.StatusBadRequest, "validation_failed", "role is personal, filing or court")
	}
	a := store.Account{Provider: prov, Role: b.Role, Address: strings.ToLower(strings.TrimSpace(b.Address)), Label: strings.TrimSpace(b.Label),
		OwnerUserID: b.OwnerUserID, CreatedBy: v.UserID, Status: "connected"}
	roleDefaults(&a)
	return a, nil
}

// connectIMAP connects an IMAP/SMTP account. The password goes straight to the core's
// sealed store; no mod_email column, log line or response holds it.
func (s *Server) connectIMAP(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	var b connectBody
	if err := decode(r, &b); err != nil {
		return err
	}
	a, err := newAccount(v, b, "imap")
	if err != nil {
		return err
	}
	if !send.ValidAddress(a.Address) {
		return fail(http.StatusBadRequest, "validation_failed", "address is not a valid email address")
	}
	if b.IMAPHost == "" || b.SMTPHost == "" || b.Login == "" || b.Password == "" {
		return fail(http.StatusBadRequest, "validation_failed", "IMAP host, SMTP host, login and password are required")
	}
	sec := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	a.IMAPHost, a.IMAPPort, a.IMAPSecurity = strings.TrimSpace(b.IMAPHost), b.IMAPPort, sec(b.IMAPSecurity, "tls")
	a.SMTPHost, a.SMTPPort, a.SMTPSecurity = strings.TrimSpace(b.SMTPHost), b.SMTPPort, sec(b.SMTPSecurity, "starttls")
	a.Login = b.Login
	if a.IMAPPort == 0 {
		a.IMAPPort = 993
	}
	if a.SMTPPort == 0 {
		a.SMTPPort = 587
	}
	for _, x := range []string{a.IMAPSecurity, a.SMTPSecurity} {
		if x != "tls" && x != "starttls" {
			return fail(http.StatusBadRequest, "validation_failed", "security is tls or starttls")
		}
	}
	if err := s.Store.InsertAccount(ctx, &a); err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			return fail(http.StatusConflict, "account_exists", "that address is already connected")
		}
		return err
	}
	if err := s.Core.PutSecret(ctx, mailbox.SecretName(a, mailbox.PasswordSecret), b.Password); err != nil {
		_, _ = s.Store.SetStatus(ctx, a.ID, "error", "the password could not be stored")
		return err
	}
	s.firstCheck(ctx, &a)
	return done(w, r, v, "/accounts", http.StatusCreated, a)
}

// firstCheck signs in once and records the result on the account.
func (s *Server) firstCheck(ctx context.Context, a *store.Account) {
	if s.CheckAccount == nil {
		return
	}
	if err := s.CheckAccount(ctx, *a); err != nil {
		status := "error"
		if errors.Is(err, provider.ErrAuth) {
			status = "needs_reauth"
		}
		_, _ = s.Store.SetStatus(ctx, a.ID, status, err.Error())
		a.Status, a.LastError = status, err.Error()
	}
}

type accountEdit struct {
	Label     string `json:"label"`
	AfterFile string `json:"after_file"`
	CanSend   *bool  `json:"can_send"`
	Plus      *bool  `json:"plus_addressing"`
}

func (s *Server) accountAction(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	a, err := s.account(ctx, v, r.PathValue("id"))
	if err != nil {
		return err
	}
	switch r.PathValue("action") {
	case "pause":
		_, err = s.Store.SetStatus(ctx, a.ID, "paused", "")
	case "resume":
		_, err = s.Store.SetStatus(ctx, a.ID, "connected", "")
	case "remove":
		for _, k := range []string{mailbox.PasswordSecret, mailbox.TokenSecret} {
			_ = s.Core.DeleteSecret(ctx, mailbox.SecretName(a, k))
		}
		a.DefaultSend, a.SystemMail = false, false
		if err = s.Store.SaveAccount(ctx, a); err == nil {
			_, err = s.Store.SetStatus(ctx, a.ID, "removed", "")
		}
	case "default-send", "system-mail":
		if !v.Admin() {
			return errForbidden
		}
		if !a.CanSend {
			return fail(http.StatusConflict, "cannot_send", "this account doesn't send")
		}
		err = s.Store.Mark(ctx, a.ID, r.PathValue("action") == "system-mail")
	case "edit":
		var b accountEdit
		if err := decode(r, &b); err != nil {
			return err
		}
		if b.Label != "" {
			a.Label = b.Label
		}
		if b.AfterFile != "" {
			a.AfterFile = b.AfterFile
		}
		if b.CanSend != nil {
			a.CanSend = *b.CanSend
		}
		if b.Plus != nil {
			a.PlusAddressing = *b.Plus
		}
		if err := s.Store.SaveAccount(ctx, a); err != nil {
			return fail(http.StatusBadRequest, "validation_failed", "that combination is refused for this account's role")
		}
	case "sync":
		if s.Syncer != nil {
			domains, st, derr := s.Syncer.Domains(ctx)
			if derr != nil {
				return derr
			}
			err = s.Syncer.SyncAccount(ctx, a, st, domains)
		}
	default:
		return errNotFound
	}
	if err != nil {
		return err
	}
	a, _ = s.Store.Account(ctx, a.ID)
	return done(w, r, v, "/accounts", http.StatusOK, a)
}

// --- OAuth ---

// publicBase is the module's address as the browser sees it, from the proxy's headers.
func publicBase(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "https"
		if r.TLS == nil && strings.HasPrefix(r.Host, "127.0.0.1") {
			proto = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return proto + "://" + host + ModulePrefix
}

type oauthStartBody struct {
	connectBody
	AccountID string `json:"account_id"` // reconnect an existing account
}

// oauthStart stores a state row bound to the user and answers the consent page's URL,
// which the tab opens in a top-level window.
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	var b oauthStartBody
	if err := decode(r, &b); err != nil {
		return err
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if b.AccountID != "" {
		a, err := s.account(ctx, v, b.AccountID)
		if err != nil {
			return err
		}
		b.Provider, b.Role, b.OwnerUserID, b.Address = a.Provider, a.Role, a.OwnerUserID, a.Address
	} else if _, err := newAccount(v, b.connectBody, b.Provider); err != nil {
		return err
	}
	clientID := st.GoogleClientID
	switch b.Provider {
	case "gmail":
	case "microsoft":
		clientID = st.MicrosoftClientID
	default:
		return fail(http.StatusBadRequest, "validation_failed", "provider is gmail or microsoft")
	}
	if clientID == "" {
		return fail(http.StatusConflict, "not_configured", "the admin has not entered this provider's app keys")
	}
	verifier, state := oauth.NewVerifier()
	owner := b.OwnerUserID
	if b.Role == "personal" && owner == "" {
		owner = v.UserID
	}
	redirect := publicBase(r) + "/oauth/callback"
	if err := s.Store.InsertState(ctx, store.OAuthState{State: state, AccountID: b.AccountID, UserID: v.UserID, Provider: b.Provider, Role: b.Role,
		OwnerUserID: owner, Label: b.Label, CodeVerifier: verifier, RedirectURI: redirect, ExpiresAt: s.now().Add(10 * time.Minute).UnixMilli()}); err != nil {
		return err
	}
	u := oauth.AuthURL(oauth.For(b.Provider, st.MicrosoftTenant), b.Provider, clientID, redirect, state, verifier, b.Address)
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
	return nil
}

// oauthCallback finishes a consent: an unknown or expired state stores nothing and
// answers 400.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	st, err := s.Store.TakeState(ctx, q.Get("state"))
	if err != nil || q.Get("state") == "" || st.ExpiresAt < s.now().UnixMilli() {
		writePage(w, http.StatusBadRequest, "Mail account", messageBody("This sign-in link is unknown or has expired. Start again from the Mail accounts tab."))
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		writePage(w, http.StatusBadRequest, "Mail account", messageBody("The provider did not allow access. Start again from the Mail accounts tab."))
		return
	}
	set, err := s.Settings(ctx)
	if err != nil {
		writePage(w, http.StatusBadGateway, "Mail account", messageBody("Togavsu can't be reached right now. Try again."))
		return
	}
	clientID, secret := set.GoogleClientID, set.GoogleClientSecret
	if st.Provider == "microsoft" {
		clientID, secret = set.MicrosoftClientID, set.MicrosoftClientSecret
	}
	tokenURL := oauth.For(st.Provider, set.MicrosoftTenant).TokenURL
	if s.OAuthToken != "" {
		tokenURL = s.OAuthToken
	}
	tok, err := oauth.Exchange(ctx, s.HTTP, tokenURL, clientID, secret, q.Get("code"), st.RedirectURI, st.CodeVerifier)
	if err != nil {
		writePage(w, http.StatusBadRequest, "Mail account", messageBody("The provider refused the sign-in. Start again from the Mail accounts tab."))
		return
	}
	var a store.Account
	if st.AccountID != "" {
		if a, err = s.Store.Account(ctx, st.AccountID); err != nil {
			writePage(w, http.StatusBadRequest, "Mail account", messageBody("That account is gone."))
			return
		}
	} else {
		address, err := s.profileAddress(ctx, st.Provider, tok)
		if err != nil {
			writePage(w, http.StatusBadGateway, "Mail account", messageBody("The provider didn't say which mailbox this is. Try again."))
			return
		}
		a = store.Account{Provider: st.Provider, Role: st.Role, Address: strings.ToLower(address), Label: st.Label, OwnerUserID: st.OwnerUserID,
			CreatedBy: st.UserID, Status: "connected"}
		roleDefaults(&a)
		if err := s.Store.InsertAccount(ctx, &a); err != nil {
			writePage(w, http.StatusConflict, "Mail account", messageBody("That address is already connected."))
			return
		}
	}
	raw, _ := json.Marshal(tok)
	if err := s.Core.PutSecret(ctx, mailbox.SecretName(a, mailbox.TokenSecret), string(raw)); err != nil {
		_, _ = s.Store.SetStatus(ctx, a.ID, "error", "the sign-in could not be stored")
		writePage(w, http.StatusBadGateway, "Mail account", messageBody("The sign-in could not be stored. Try again."))
		return
	}
	if a.Status != "paused" {
		_, _ = s.Store.SetStatus(ctx, a.ID, "connected", "")
		a.Status = "connected"
	}
	s.firstCheck(ctx, &a)
	writePage(w, http.StatusOK, "Mail account", messageBody(a.Address+" is connected ("+a.Status+"). You can close this window."))
}

// profileAddress asks the provider which mailbox the token is for.
func (s *Server) profileAddress(ctx context.Context, prov string, tok provider.Token) (string, error) {
	return oauth.Profile(ctx, s.HTTP, prov, s.APIBase[prov], tok.AccessToken)
}

func (s *Server) getPrefs(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	p, err := s.Store.Prefs(r.Context(), v.UserID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (s *Server) savePrefs(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	var b struct {
		NotifyUnfiled bool   `json:"notify_unfiled"`
		Signature     string `json:"signature_text"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	p := store.Prefs{UserID: v.UserID, NotifyUnfiled: b.NotifyUnfiled, SignatureText: b.Signature}
	if err := s.Store.SavePrefs(r.Context(), p); err != nil {
		return err
	}
	return done(w, r, v, "/accounts", http.StatusOK, p)
}
