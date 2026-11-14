package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"text/template"

	outlookaddin "github.com/CharitonMedia/togavsu-email/outlook-addin"

	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/mailparse"
	"github.com/CharitonMedia/togavsu-email/internal/match"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/store"
	"github.com/CharitonMedia/togavsu-email/internal/syncer"
)

// DeviceHeader carries the add-in's device grant secret.
const DeviceHeader = "X-Togavsu-Device-Grant"

// addin wraps an add-in API call: the device grant is exchanged for a context token on
// every request (CC-7), so a revoked grant gets 401 and does nothing.
func (s *Server) addin(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := r.Header.Get(DeviceHeader)
		if secret == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "detail": "sign in to Togavsu from the add-in"})
			return
		}
		t, err := s.Core.ExchangeDevice(r.Context(), secret, "")
		if err != nil {
			if coreapi.StatusOf(err) == http.StatusUnauthorized || coreapi.StatusOf(err) == http.StatusForbidden && !coreapi.IsScopeDenied(err) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "detail": "this device is no longer connected"})
				return
			}
			if coreapi.IsScopeDenied(err) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "detail": "the Outlook add-in is not enabled for this firm"})
				return
			}
			s.writeErr(w, r, err)
			return
		}
		v := Viewer{ViewerInfo: coreapi.ViewerInfo{UserID: t.ViewerUserID, Roles: t.Roles}, Token: t.Token}
		if !v.Staff() {
			writeJSON(w, http.StatusForbidden, map[string]string{"code": "forbidden", "detail": "the add-in is for firm staff"})
			return
		}
		if err := h(w, r, v); err != nil {
			s.writeErr(w, r, err)
		}
	}
}

func (s *Server) addinPing(w http.ResponseWriter, _ *http.Request, v Viewer) error {
	writeJSON(w, http.StatusOK, map[string]any{"user_id": v.UserID, "roles": v.Roles})
	return nil
}

type addinMatter struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Client string `json:"client,omitempty"`
}

func fromMatch(m coreapi.MatchMatter) addinMatter {
	var cl []string
	for _, c := range m.Clients {
		cl = append(cl, strings.TrimSpace(c.GivenName+" "+c.FamilyName))
	}
	return addinMatter{ID: m.ID, Number: m.Number, Name: m.Name, Status: m.Status, Client: strings.Join(cl, ", ")}
}

// addinSuggest is Unified's suggest-matter: the address rules over the core's viewer
// match, so a walled matter comes back only to a user on its wall.
func (s *Server) addinSuggest(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	var b struct {
		From       string   `json:"from"`
		To         []string `json:"to"`
		Cc         []string `json:"cc"`
		Subject    string   `json:"subject"`
		InternetID string   `json:"internet_message_id"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	if b.InternetID != "" {
		if mid, err := s.Store.FiledMatter(ctx, b.InternetID); err == nil && mid != "" {
			if m, err := s.Core.GetMatter(ctx, v.Auth(), mid); err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"filed": addinMatter{ID: m.ID, Number: m.Number, Name: m.Name, Status: m.Status}})
				return nil
			}
		}
	}
	domains, _, err := s.Syncer.Domains(ctx)
	if err != nil {
		return err
	}
	in := match.Input{Role: "filing", FirmDomains: domains, From: send.Addresses([]string{b.From}), To: send.Addresses(b.To), Cc: send.Addresses(b.Cc), Subject: b.Subject}
	ans, err := s.Core.MatchMail(ctx, v.Auth(), match.Request(in, match.Config{}))
	if err != nil {
		return err
	}
	d, err := match.Decide(ctx, in, match.Config{}, ans, nil)
	if err != nil {
		return err
	}
	out := map[string]any{"reason": d.Reason, "candidates": []addinMatter{}}
	var cands []addinMatter
	for _, m := range ans.Matters {
		if d.Outcome == match.File && m.ID == d.MatterID {
			out["suggested"] = fromMatch(m)
			continue
		}
		if m.HasVia("address") {
			cands = append(cands, fromMatch(m))
		}
	}
	if cands != nil {
		out["candidates"] = cands
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) addinMatters(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ms, err := s.Core.ListMatters(r.Context(), v.Auth(), r.URL.Query().Get("q"), 20)
	if err != nil {
		return err
	}
	out := []addinMatter{}
	for _, m := range ms {
		out = append(out, addinMatter{ID: m.ID, Number: m.Number, Name: m.Name, Status: m.Status})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func (s *Server) addinContacts(w http.ResponseWriter, r *http.Request, v Viewer) error {
	cs, err := s.Core.SearchContacts(r.Context(), v.Auth(), r.URL.Query().Get("q"))
	if err != nil {
		return err
	}
	if cs == nil {
		cs = []coreapi.Contact{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": cs})
	return nil
}

// addinAccount is the account an add-in item is recorded under: the user's own
// account the message was to or from, else their first, else the default send account.
func (s *Server) addinAccount(ctx context.Context, v Viewer, pm *mailparse.Message) (store.Account, error) {
	own, err := s.Store.PersonalAccounts(ctx, v.UserID)
	if err != nil {
		return store.Account{}, err
	}
	all := append(append(append([]string{}, pm.From...), pm.To...), pm.Cc...)
	for _, a := range own {
		for _, x := range all {
			if strings.EqualFold(x, a.Address) {
				return a, nil
			}
		}
	}
	if len(own) > 0 {
		return own[0], nil
	}
	if a, err := s.Store.DefaultSendAccount(ctx); err == nil {
		return a, nil
	}
	return store.Account{}, fail(http.StatusConflict, "no_account", "connect your mailbox on the Mail accounts tab first")
}

// addinFile files the item Outlook hands over (getAsFileAsync) as section 8.4 with how
// addin. The same item filed twice is one filing.
func (s *Server) addinFile(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	var b struct {
		MatterID     string `json:"matter_id"`
		RawBase64    string `json:"raw_base64"`
		AttorneyOnly bool   `json:"attorney_only"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(b.RawBase64)
	if err != nil || len(raw) == 0 {
		return fail(http.StatusBadRequest, "validation_failed", "raw_base64 is the message from getAsFileAsync")
	}
	pm, err := mailparse.Parse(raw)
	if err != nil {
		return fail(http.StatusBadRequest, "validation_failed", "the message could not be read")
	}
	if _, err := s.Core.GetMatter(ctx, v.Auth(), b.MatterID); err != nil {
		return err
	}
	// Already filed (from the add-in, a sync or the queue): the same filing.
	if existing, err := s.Store.MessageByRFC(ctx, pm.MessageID); err == nil {
		if f, err := s.Store.Filing(ctx, existing.ID); err == nil {
			if f.MatterID == b.MatterID {
				writeJSON(w, http.StatusOK, f)
				return nil
			}
		}
	}
	a, err := s.addinAccount(ctx, v, pm)
	if err != nil {
		return err
	}
	domains, _, err := s.Syncer.Domains(ctx)
	if err != nil {
		return err
	}
	dir := "inbound"
	if match.Direction(match.Input{Role: a.Role, FirmDomains: domains, From: pm.From}) == "outbound" {
		dir = "outbound"
	}
	m := syncer.Record(pm, a, store.Folder{}, "addin:"+pm.RawSHA256, dir)
	m.Unread = false
	if _, err := s.Store.Claim(ctx, &m); err != nil {
		return err
	}
	stored, err := s.Store.Message(ctx, m.ID)
	if errors.Is(err, store.ErrNotFound) {
		// The claim lost to an earlier upload of the same item: use that row.
		rows, qerr := s.Store.ByRef(ctx, a.ID, "addin:"+pm.RawSHA256)
		if qerr != nil {
			return qerr
		}
		stored = rows
	} else if err != nil {
		return err
	}
	if err := s.Store.SetParts(ctx, stored.ID, send.PartsOf(pm)); err != nil {
		return err
	}
	f, err := s.FileAs(ctx, v, stored, b.MatterID, "addin", b.AttorneyOnly, false, raw)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, f)
	return nil
}

// addinPlus is the filing account's plus address for a matter (Unified's clients+TOKEN).
func (s *Server) addinPlus(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	m, err := s.Core.GetMatter(ctx, v.Auth(), r.URL.Query().Get("matter_id"))
	if err != nil {
		return err
	}
	as, err := s.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range as {
		if a.Role == "filing" && a.PlusAddressing && a.Status != "removed" {
			at := strings.LastIndexByte(a.Address, '@')
			writeJSON(w, http.StatusOK, map[string]string{"address": a.Address[:at] + "+" + strings.ToLower(m.Number) + a.Address[at:]})
			return nil
		}
	}
	return fail(http.StatusNotFound, "not_found", "no filing account takes plus addresses")
}

var manifestTpl = template.Must(template.ParseFS(outlookaddin.Files, "manifest.xml"))

// addinManifest fills the manifest with the firm's public address from the request host.
func (s *Server) addinManifest(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Admin() {
		return errNotFound
	}
	base := publicBase(r)
	u, _ := url.Parse(base)
	var buf bytes.Buffer
	if err := manifestTpl.Execute(&buf, map[string]string{"Base": base, "Origin": u.Scheme + "://" + u.Host, "ID": addinID(u.Host)}); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="manifest.xml"`)
	_, _ = w.Write(buf.Bytes())
	return nil
}

// addinID is a stable add-in id for a firm's host, so a redeploy updates the add-in.
func addinID(host string) string {
	h := mailparse.Sum([]byte("togavsu-email-addin:" + host))
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

var addinTypes = map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".png": "image/png"}

// addinFile serves the add-in's static files (the manifest goes through addinManifest).
func addinFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ct, ok := addinTypes[path.Ext(name)]
	if !ok || name == "manifest.xml" {
		http.NotFound(w, r)
		return
	}
	b, err := outlookaddin.Files.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}
