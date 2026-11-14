package server

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/render"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

func ownAccountIDs(as []store.Account) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

// readable: a message the viewer may see in the queue or inbox, or one filed to a
// matter the core lets the viewer read.
func (s *Server) readable(ctx context.Context, v Viewer, id string) (store.Message, string, error) {
	m, err := s.Store.Message(ctx, id)
	if err != nil {
		return store.Message{}, "", errNotFound
	}
	label := ""
	if f, err := s.Store.Filing(ctx, m.ID); err == nil {
		if mt, err := s.Core.GetMatter(ctx, v.Auth(), f.MatterID); err == nil {
			if f.AttorneyOnly && !v.Lawyer() {
				return store.Message{}, "", errNotFound
			}
			return m, mt.Number + " " + mt.Name, nil
		}
		if m.Role == "personal" && m.OwnerUserID == v.UserID {
			return m, label, nil
		}
		return store.Message{}, "", errNotFound
	}
	if !mayView(v, m) {
		return store.Message{}, "", errNotFound
	}
	return m, label, nil
}

func (s *Server) inboxPage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	ctx := r.Context()
	folder := r.URL.Query().Get("folder")
	if folder != "sent" {
		folder = "inbox"
	}
	own, err := s.Store.PersonalAccounts(ctx, v.UserID)
	if err != nil {
		return err
	}
	ids := ownAccountIDs(own)
	rows, err := s.Store.Inbox(ctx, ids, folder, inboxRows)
	if err != nil {
		return err
	}
	unread, err := s.Store.UnreadCount(ctx, ids)
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, inboxTpl, map[string]any{"Title": "Mail", "Ctx": v.Token, "Folder": folder, "Rows": rows, "Unread": unread, "HasAccounts": len(own) > 0})
}

func (s *Server) inboxAPI(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	own, err := s.Store.PersonalAccounts(r.Context(), v.UserID)
	if err != nil {
		return err
	}
	folder := r.URL.Query().Get("folder")
	if folder != "sent" {
		folder = "inbox"
	}
	rows, err := s.Store.Inbox(r.Context(), ownAccountIDs(own), folder, inboxRows)
	if err != nil {
		return err
	}
	if rows == nil {
		rows = []store.Message{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows, "folder": folder})
	return nil
}

func (s *Server) messagePage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	m, matter, err := s.readable(r.Context(), v, r.PathValue("id"))
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, messageViewTpl, map[string]any{"Title": m.Subject, "Ctx": v.Token, "M": m, "Matter": matter, "CanAct": mayView(v, m)})
}

func (s *Server) messageAPI(w http.ResponseWriter, r *http.Request, v Viewer) error {
	m, matter, err := s.readable(r.Context(), v, r.PathValue("id"))
	if err != nil {
		return err
	}
	parts, err := s.Store.Parts(r.Context(), m.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": m, "parts": parts, "matter": matter})
	return nil
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	m, err := s.visibleMessage(ctx, v, r.PathValue("id"))
	if err != nil {
		return err
	}
	var b struct {
		Read bool `json:"read"`
	}
	if err := decode(r, &b); err != nil {
		return err
	}
	a, err := s.Store.Account(ctx, m.AccountID)
	if err != nil {
		return err
	}
	if atProvider(m) && a.Status != "paused" {
		p, err := s.Open(ctx, a)
		if err != nil {
			return err
		}
		if err := p.MarkRead(ctx, provider.ParseRef(m.ProviderRef), b.Read); err != nil {
			return err
		}
	}
	if err := s.Store.SetUnread(ctx, m.ID, !b.Read); err != nil {
		return err
	}
	return done(w, r, v, "/messages/"+m.ID, http.StatusOK, map[string]any{"id": m.ID, "unread": !b.Read})
}

// preview is the sanitized message as a self-contained document with a script-free,
// sandboxed policy.
func (s *Server) preview(w http.ResponseWriter, r *http.Request, v Viewer) error {
	m, _, err := s.readable(r.Context(), v, r.PathValue("id"))
	if err != nil {
		return err
	}
	var date = m.ReceivedAt
	if m.SentAt != nil {
		date = *m.SentAt
	}
	doc := render.Document(render.Header{From: m.FromHeader, To: strings.Join(m.ToAddrs, ", "), Cc: strings.Join(m.CcAddrs, ", "), Subject: m.Subject,
		Date: msTime(date)}, render.Sanitize(m.BodyHTML), m.BodyText)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", render.PreviewCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(doc))
	return nil
}

func (s *Server) unfiledPage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	filter, account := r.URL.Query().Get("filter"), r.URL.Query().Get("account_id")
	ms, err := s.queue(ctx, v, filter, account, listMax)
	if err != nil {
		return err
	}
	var rows []QueueRow
	for _, m := range ms {
		row, err := s.row(ctx, v, m, one)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	return renderPage(w, http.StatusOK, unfiledTpl, map[string]any{"Title": "Unfiled email", "Ctx": v.Token, "Rows": rows, "Filter": filter, "AccountID": account})
}

func (s *Server) fileDialog(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	m, err := s.visibleMessage(ctx, v, r.PathValue("id"))
	if err != nil {
		return err
	}
	row, err := s.row(ctx, v, m, zero)
	if err != nil {
		return err
	}
	q := r.URL.Query().Get("q")
	var found []coreapi.Matter
	if q != "" {
		if found, err = s.Core.ListMatters(ctx, v.Auth(), q, matterPickRows); err != nil {
			return err
		}
	}
	return renderPage(w, http.StatusOK, fileTpl, map[string]any{"Title": "File email", "Ctx": v.Token, "M": m, "Row": row, "Q": q, "Found": found, "Lawyer": v.Lawyer()})
}

func (s *Server) accountsPage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	as, err := s.visibleAccounts(r.Context(), v)
	if err != nil {
		return err
	}
	p, err := s.Store.Prefs(r.Context(), v.UserID)
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, accountsTpl, map[string]any{"Title": "Mail accounts", "Ctx": v.Token, "Accounts": as, "Admin": v.Admin(), "Prefs": p})
}

// Thread is a matter's filings in one thread.
type Thread struct {
	Key      string         `json:"thread_key"`
	Subject  string         `json:"subject"`
	Filings  []store.Filing `json:"filings"`
	LastMail int64          `json:"last_mail_at"`
}

func threads(fs []store.Filing) []Thread {
	by := map[string]*Thread{}
	var order []string
	for _, f := range fs {
		k := f.ThreadKey
		if k == "" {
			k = f.RFCMessageID
		}
		t, ok := by[k]
		if !ok {
			t = &Thread{Key: k, Subject: f.Subject}
			by[k] = t
			order = append(order, k)
		}
		t.Filings = append(t.Filings, f)
		if f.FiledAt > t.LastMail {
			t.LastMail = f.FiledAt
		}
	}
	out := []Thread{}
	for _, k := range order {
		t := by[k]
		sort.Slice(t.Filings, func(i, j int) bool { return t.Filings[i].FiledAt < t.Filings[j].FiledAt })
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastMail > out[j].LastMail })
	return out
}

// matterFilings reads a matter's filings once the core confirms the viewer may read
// the matter; attorney-only filings are left out for anyone but attorneys and admins.
func (s *Server) matterFilings(ctx context.Context, v Viewer, matterID string, limit int) ([]store.Filing, error) {
	if matterID == "" {
		return nil, fail(http.StatusBadRequest, "validation_failed", "open this from a matter")
	}
	if _, err := s.Core.GetMatter(ctx, v.Auth(), matterID); err != nil {
		return nil, err
	}
	fs, err := s.Store.MatterFilings(ctx, matterID, limit)
	if err != nil {
		return nil, err
	}
	out := []store.Filing{}
	for _, f := range fs {
		if f.AttorneyOnly && !v.Lawyer() {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *Server) matterPage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	id := v.Matter
	if id == "" {
		id = r.URL.Query().Get("matter_id")
	}
	fs, err := s.matterFilings(r.Context(), v, id, listMax)
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, matterTpl, map[string]any{"Title": "Mail", "Ctx": v.Token, "Threads": threads(fs), "MatterID": id})
}

func (s *Server) recentPage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	id := v.Matter
	if id == "" {
		id = r.URL.Query().Get("matter_id")
	}
	fs, err := s.matterFilings(r.Context(), v, id, matterRecentRows)
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, recentTpl, map[string]any{"Title": "Recent mail", "Ctx": v.Token, "Filings": fs})
}

func (s *Server) matterMail(w http.ResponseWriter, r *http.Request, v Viewer) error {
	fs, err := s.matterFilings(r.Context(), v, r.PathValue("id"), listMax)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads(fs)})
	return nil
}

// primaryClientAddress finds the matter's primary client's primary address.
func (s *Server) primaryClientAddress(ctx context.Context, v Viewer, matterID string) string {
	ps, err := s.Core.Parties(ctx, v.Auth(), matterID)
	if err != nil {
		return ""
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Primary && !ps[j].Primary })
	for _, p := range ps {
		if p.Role != "client" {
			continue
		}
		es, err := s.Core.ContactEmails(ctx, v.Auth(), p.ContactID)
		if err != nil {
			continue
		}
		sort.SliceStable(es, func(i, j int) bool { return es[i].Primary && !es[j].Primary })
		for _, e := range es {
			if e.EffectiveTo == nil {
				return e.Email
			}
		}
	}
	return ""
}

func quote(m store.Message) string {
	var b strings.Builder
	b.WriteString("\n\nOn " + msTime(m.ReceivedAt).Format("Jan 2, 2006") + ", " + m.FromHeader + " wrote:\n")
	for _, l := range strings.Split(strings.TrimSpace(m.BodyText), "\n") {
		b.WriteString("> " + l + "\n")
	}
	return b.String()
}

func (s *Server) composePage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	ctx := r.Context()
	q := r.URL.Query()
	d := map[string]any{"Title": "New email", "Heading": "New email", "Ctx": v.Token, "Kind": "quick", "Key": "c-" + randomKey()}
	if q.Get("sent") != "" {
		d["Error"] = "Sent."
	}
	matterID := q.Get("matter_id")
	if matterID == "" {
		matterID = v.Matter
	}
	if id := q.Get("reply_to"); id != "" {
		m, err := s.visibleMessage(ctx, v, id)
		if err != nil {
			return err
		}
		kind := q.Get("kind")
		if kind != "forward" {
			kind = "reply"
		}
		d["Kind"], d["ReplyTo"] = kind, m.ID
		subj := m.Subject
		if kind == "reply" {
			if !strings.HasPrefix(strings.ToLower(subj), "re:") {
				subj = "Re: " + subj
			}
			to := []string{m.FromAddr}
			if q.Get("all") != "" {
				own := map[string]bool{strings.ToLower(m.AccountAddress): true}
				var cc []string
				for _, a := range append(append([]string{}, m.ToAddrs...), m.CcAddrs...) {
					if !own[a] && a != m.FromAddr {
						cc = append(cc, a)
					}
				}
				d["Cc"] = strings.Join(cc, ", ")
			}
			d["To"] = strings.Join(to, ", ")
			d["Heading"] = "Reply"
		} else {
			if !strings.HasPrefix(strings.ToLower(subj), "fwd:") {
				subj = "Fwd: " + subj
			}
			d["Heading"] = "Forward"
		}
		d["Subject"], d["Body"] = subj, quote(m)
		if f, err := s.Store.Filing(ctx, m.ID); err == nil && matterID == "" {
			matterID = f.MatterID
		}
	}
	if matterID != "" {
		mt, err := s.Core.GetMatter(ctx, v.Auth(), matterID)
		if err != nil {
			return err
		}
		d["Matter"] = mt
		if files, err := s.Core.ListFiles(ctx, v.Auth(), matterID); err == nil {
			d["Files"] = files
		}
		if d["To"] == nil && d["ReplyTo"] == nil {
			d["To"] = s.primaryClientAddress(ctx, v, matterID)
		}
	}
	if own, err := s.Store.PersonalAccounts(ctx, v.UserID); err == nil {
		var can []store.Account
		for _, a := range own {
			if a.CanSend {
				can = append(can, a)
			}
		}
		d["Accounts"] = can
	}
	if p, err := s.Store.Prefs(ctx, v.UserID); err == nil && p.SignatureText != "" && d["Body"] == nil {
		d["Body"] = "\n\n-- \n" + p.SignatureText
	}
	return renderPage(w, http.StatusOK, composeTpl, d)
}

type sendBody struct {
	Kind           string   `json:"kind"`
	AccountID      string   `json:"account_id"`
	To             any      `json:"to"`
	Cc             any      `json:"cc"`
	Bcc            any      `json:"bcc"`
	Subject        string   `json:"subject"`
	Body           string   `json:"body"`
	MatterID       string   `json:"matter_id"`
	FileIDs        []string `json:"file_ids"`
	ReplyTo        string   `json:"reply_to"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func strList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func (s *Server) sendAPI(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	var b sendBody
	if err := decode(r, &b); err != nil {
		return err
	}
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		b.IdempotencyKey = k
	}
	res, err := s.Sender.Send(r.Context(), v.send(), send.Compose{Kind: b.Kind, AccountID: b.AccountID, To: strList(b.To), Cc: strList(b.Cc), Bcc: strList(b.Bcc),
		Subject: b.Subject, Body: b.Body, MatterID: strings.TrimSpace(b.MatterID), FileIDs: b.FileIDs, ReplyTo: b.ReplyTo, IdempotencyKey: b.IdempotencyKey})
	if err != nil {
		return err
	}
	return done(w, r, v, "/compose?sent=1", http.StatusOK, res)
}

func (s *Server) inboxWidget(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	ctx := r.Context()
	own, err := s.Store.PersonalAccounts(ctx, v.UserID)
	if err != nil {
		return err
	}
	folder := r.URL.Query().Get("folder")
	if folder != "sent" {
		folder = "inbox"
	}
	ids := ownAccountIDs(own)
	rows, err := s.Store.Inbox(ctx, ids, folder, inboxWidgetRows)
	if err != nil {
		return err
	}
	unread, err := s.Store.UnreadCount(ctx, ids)
	if err != nil {
		return err
	}
	q, err := s.queue(ctx, v, "", "", zero)
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, widgetInboxTpl, map[string]any{"Title": "Inbox", "Rows": rows, "Unread": unread, "Unfiled": len(q), "HasAccounts": len(own) > 0})
}

func (s *Server) unfiledWidget(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	ms, err := s.queue(ctx, v, "", r.URL.Query().Get("account_id"), unfiledWidgetRows)
	if err != nil {
		return err
	}
	var rows []QueueRow
	for _, m := range ms {
		row, err := s.row(ctx, v, m, one)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	return renderPage(w, http.StatusOK, widgetUnfiledTpl, map[string]any{"Title": "Unfiled email", "Rows": rows})
}

// Search matches subject, from, to and body. Filed messages come back only when the
// core lets the viewer read their matter; personal mail only to its owner; firm queue
// rows only to staff.
func (s *Server) Search(ctx context.Context, v Viewer, q string) ([]store.Message, error) {
	out := []store.Message{}
	if strings.TrimSpace(q) == "" || !v.Staff() {
		return out, nil
	}
	ms, err := s.Store.Search(ctx, q, listMax)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	filed, err := s.Store.FilingsOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	canRead := map[string]bool{}
	for _, m := range ms {
		if len(out) >= SearchLimit {
			break
		}
		if f, ok := filed[m.ID]; ok {
			seen, known := canRead[f.MatterID]
			if !known {
				_, err := s.Core.GetMatter(ctx, v.Auth(), f.MatterID)
				seen = err == nil
				canRead[f.MatterID] = seen
			}
			if seen && (!f.AttorneyOnly || v.Lawyer()) {
				out = append(out, m)
			}
			continue
		}
		if m.Role == "personal" {
			if m.OwnerUserID == v.UserID {
				out = append(out, m)
			}
			continue
		}
		if m.Status == "unfiled" || m.Status == "deferred" || m.Status == "error" {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request, v Viewer) error {
	q := r.URL.Query().Get("q")
	rows, err := s.Search(r.Context(), v, q)
	if err != nil {
		return err
	}
	return renderPage(w, http.StatusOK, searchTpl, map[string]any{"Title": "Search email", "Ctx": v.Token, "Q": q, "Rows": rows})
}

func (s *Server) searchAPI(w http.ResponseWriter, r *http.Request, v Viewer) error {
	rows, err := s.Search(r.Context(), v, r.URL.Query().Get("q"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
	return nil
}
