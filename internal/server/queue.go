package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/filing"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// mayView applies the queue's visibility: firm and court rows to admins, attorneys and
// staff; personal rows to their owner alone. Anything else is not found.
func mayView(v Viewer, m store.Message) bool {
	if m.Role == "personal" {
		return m.OwnerUserID == v.UserID
	}
	return v.Staff()
}

func (s *Server) visibleMessage(ctx context.Context, v Viewer, id string) (store.Message, error) {
	m, err := s.Store.Message(ctx, id)
	if err != nil || !mayView(v, m) {
		return store.Message{}, errNotFound
	}
	return m, nil
}

// Suggestion is a candidate matter the viewer may see.
type Suggestion struct {
	MatterID string `json:"matter_id"`
	Number   string `json:"number"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Via      string `json:"via"`
}

// QueueRow is one row of the unfiled queue.
type QueueRow struct {
	ID          string       `json:"id"`
	Account     string       `json:"account"`
	AccountID   string       `json:"account_id"`
	From        string       `json:"from"`
	Subject     string       `json:"subject"`
	ReceivedAt  int64        `json:"received_at"`
	Attachments int          `json:"attachment_count"`
	Reason      string       `json:"reason"`
	Error       string       `json:"error,omitempty"`
	Status      string       `json:"status"`
	Suggestions []Suggestion `json:"suggestions"`
	// FileWithoutAttachments is offered when the raw message is gone.
	FileWithoutAttachments bool `json:"file_without_attachments"`
}

// suggestions reads each saved candidate as the viewer; a matter the viewer may not
// see (a wall) is left out, name and all.
func (s *Server) suggestions(ctx context.Context, v Viewer, messageID string, limit int) ([]Suggestion, error) {
	ms, err := s.Store.Matches(ctx, messageID)
	if err != nil {
		return nil, err
	}
	out := []Suggestion{}
	for _, c := range ms {
		if limit > 0 && len(out) >= limit {
			break
		}
		m, err := s.Core.GetMatter(ctx, v.Auth(), c.MatterID)
		if err != nil {
			if coreapi.IsNotFound(err) || coreapi.StatusOf(err) == http.StatusForbidden {
				continue
			}
			return nil, err
		}
		out = append(out, Suggestion{MatterID: m.ID, Number: m.Number, Name: m.Name, Status: m.Status, Via: c.Via})
	}
	return out, nil
}

func (s *Server) row(ctx context.Context, v Viewer, m store.Message, limit int) (QueueRow, error) {
	parts, err := s.Store.Parts(ctx, m.ID)
	if err != nil {
		return QueueRow{}, err
	}
	n := 0
	for _, p := range parts {
		if !p.Inline {
			n++
		}
	}
	sug, err := s.suggestions(ctx, v, m.ID, limit)
	if err != nil {
		return QueueRow{}, err
	}
	from := m.FromHeader
	if from == "" {
		from = m.FromAddr
	}
	return QueueRow{ID: m.ID, Account: m.AccountAddress, AccountID: m.AccountID, From: from, Subject: m.Subject, ReceivedAt: m.ReceivedAt,
		Attachments: n, Reason: m.MatchReason, Error: m.Error, Status: m.Status, Suggestions: sug,
		FileWithoutAttachments: m.Error == filing.NotInMailbox}, nil
}

func queueStatuses(filter string) []string {
	if filter == "deferred" {
		return []string{"deferred"}
	}
	return []string{"unfiled", "error"}
}

func (s *Server) queue(ctx context.Context, v Viewer, filter, account string, limit int) ([]store.Message, error) {
	return s.Store.Queue(ctx, store.QueueFilter{Statuses: queueStatuses(filter), AccountID: account, Firm: v.Staff(), Owner: v.UserID, Limit: limit})
}

func (s *Server) queueList(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ms, err := s.queue(r.Context(), v, r.URL.Query().Get("filter"), r.URL.Query().Get("account_id"), 500)
	if err != nil {
		return err
	}
	rows := []QueueRow{}
	for _, m := range ms {
		row, err := s.row(r.Context(), v, m, 1)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
	return nil
}

func (s *Server) queueRow(w http.ResponseWriter, r *http.Request, v Viewer) error {
	m, err := s.visibleMessage(r.Context(), v, r.PathValue("id"))
	if err != nil {
		return err
	}
	row, err := s.row(r.Context(), v, m, 0)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, row)
	return nil
}

// badge counts the queue rows the viewer may see (the sidebar's unfiled count).
func (s *Server) badge(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ms, err := s.queue(r.Context(), v, "", "", 0)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": len(ms)})
	return nil
}

type fileBody struct {
	MatterID           string `json:"matter_id"`
	AttorneyOnly       bool   `json:"attorney_only"`
	WithoutAttachments bool   `json:"without_attachments"`
	Reason             string `json:"reason"`
	Confirm            int    `json:"confirm_count"`
	Filter             string `json:"filter"`
	AccountID          string `json:"account_id"`
}

// FileAs files a message to a matter as the viewer: the core checks the viewer may
// write to the matter, and the files and note are the viewer's.
func (s *Server) FileAs(ctx context.Context, v Viewer, m store.Message, matterID, how string, attorneyOnly, withoutAttachments bool, raw []byte) (store.Filing, error) {
	if matterID == "" {
		return store.Filing{}, fail(http.StatusBadRequest, "validation_failed", "choose a matter")
	}
	if attorneyOnly && !v.Lawyer() {
		return store.Filing{}, fail(http.StatusForbidden, "forbidden", "only an attorney or admin files attorney-only")
	}
	if _, err := s.Core.GetMatter(ctx, v.Auth(), matterID); err != nil {
		return store.Filing{}, err
	}
	a, err := s.Store.Account(ctx, m.AccountID)
	if err != nil {
		return store.Filing{}, err
	}
	f, err := s.Filer.File(ctx, filing.Request{Message: m, Account: a, MatterID: matterID, How: how, Auth: v.Auth(), FiledBy: v.UserID,
		AttorneyOnly: attorneyOnly, WithoutAttachments: withoutAttachments, Raw: raw, SkipAfterFile: a.Status == "paused"})
	switch {
	case errors.Is(err, filing.ErrNotInMailbox):
		return f, fail(http.StatusConflict, filing.NotInMailbox, "the message is gone from the mailbox; file it without attachments")
	case errors.Is(err, filing.ErrDuplicate):
		return f, nil
	}
	return f, err
}

func (s *Server) queueAction(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	m, err := s.visibleMessage(ctx, v, r.PathValue("id"))
	if err != nil {
		return err
	}
	var b fileBody
	if err := decode(r, &b); err != nil {
		return err
	}
	back := "/unfiled"
	switch r.PathValue("action") {
	case "file":
		f, err := s.FileAs(ctx, v, m, b.MatterID, "queue", b.AttorneyOnly, b.WithoutAttachments, nil)
		if err != nil {
			return err
		}
		return done(w, r, v, back, http.StatusOK, f)
	case "defer":
		if err := s.deferOne(ctx, v, m); err != nil {
			return err
		}
	case "restore":
		if m.Status != "deferred" {
			return fail(http.StatusConflict, "not_deferred", "only a deferred message is restored")
		}
		if err := s.Store.SetMessageStatus(ctx, m.ID, "unfiled", m.MatchReason, m.Error); err != nil {
			return err
		}
		if err := s.Store.QueueAction(ctx, m.ID, "restore", "", v.UserID); err != nil {
			return err
		}
	case "dismiss":
		if strings.TrimSpace(b.Reason) == "" {
			return fail(http.StatusBadRequest, "validation_failed", "dismissing needs a reason")
		}
		if err := s.dismiss(ctx, m); err != nil {
			return err
		}
		if err := s.Store.SetMessageStatus(ctx, m.ID, "dismissed", m.MatchReason, ""); err != nil {
			return err
		}
		if err := s.Store.QueueAction(ctx, m.ID, "dismiss", strings.TrimSpace(b.Reason), v.UserID); err != nil {
			return err
		}
	case "retry":
		if err := s.retry(ctx, m); err != nil {
			return err
		}
		if err := s.Store.QueueAction(ctx, m.ID, "retry", "", v.UserID); err != nil {
			return err
		}
	default:
		return errNotFound
	}
	after, _ := s.Store.Message(ctx, m.ID)
	return done(w, r, v, back, http.StatusOK, map[string]string{"id": m.ID, "status": after.Status})
}

func atProvider(m store.Message) bool {
	return m.ProviderRef != "" && !strings.HasPrefix(m.ProviderRef, "sent:") && !strings.HasPrefix(m.ProviderRef, "addin:")
}

// deferOne moves the message to Review-Later at the provider and sets it deferred;
// sync never re-queues it, since Review-Later is never read.
func (s *Server) deferOne(ctx context.Context, v Viewer, m store.Message) error {
	if m.Status != "unfiled" && m.Status != "error" {
		return fail(http.StatusConflict, "not_in_queue", "only a queue row is deferred")
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
		nr, err := p.Move(ctx, provider.ParseRef(m.ProviderRef), provider.ReviewLaterFolder)
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			return err
		}
		if err == nil {
			if err := s.Store.MoveRef(ctx, m.ID, "", nr.String()); err != nil {
				return err
			}
		}
	}
	if err := s.Store.SetMessageStatus(ctx, m.ID, "deferred", m.MatchReason, m.Error); err != nil {
		return err
	}
	return s.Store.QueueAction(ctx, m.ID, "defer", "", v.UserID)
}

// dismiss: a filing account deletes the message; a court account marks it read; a
// personal account leaves it.
func (s *Server) dismiss(ctx context.Context, m store.Message) error {
	a, err := s.Store.Account(ctx, m.AccountID)
	if err != nil {
		return err
	}
	if !atProvider(m) || a.Role == "personal" || a.Status == "paused" {
		return nil
	}
	p, err := s.Open(ctx, a)
	if err != nil {
		return err
	}
	ref := provider.ParseRef(m.ProviderRef)
	switch a.Role {
	case "filing":
		err = p.AfterFile(ctx, ref, provider.AfterFileAction{Kind: "delete"})
	case "court":
		err = p.MarkRead(ctx, ref, true)
	}
	if errors.Is(err, provider.ErrNotFound) {
		return nil
	}
	return err
}

// retry reads an error row again, or runs a queue row through the rules again.
func (s *Server) retry(ctx context.Context, m store.Message) error {
	a, err := s.Store.Account(ctx, m.AccountID)
	if err != nil {
		return err
	}
	domains, st, err := s.Syncer.Domains(ctx)
	if err != nil {
		return err
	}
	if m.Status == "error" || m.RawSHA256 == "" {
		p, err := s.Open(ctx, a)
		if err != nil {
			return err
		}
		return s.Syncer.Reread(ctx, p, a, m, st, domains)
	}
	if m.Status != "unfiled" {
		return fail(http.StatusConflict, "not_in_queue", "only a queue row is retried")
	}
	_, err = s.Syncer.Processor.Process(ctx, m, a, st, domains)
	return err
}

// deferAll defers every row in view once the confirmation names their count.
func (s *Server) deferAll(w http.ResponseWriter, r *http.Request, v Viewer) error {
	ctx := r.Context()
	var b fileBody
	if err := decode(r, &b); err != nil {
		return err
	}
	ms, err := s.queue(ctx, v, b.Filter, b.AccountID, 0)
	if err != nil {
		return err
	}
	if b.Confirm != len(ms) {
		writeJSON(w, http.StatusConflict, map[string]any{"code": "confirm_count", "detail": "confirm the number of rows to defer", "count": len(ms)})
		return nil
	}
	n := 0
	for _, m := range ms {
		if m.Status == "deferred" {
			continue
		}
		if err := s.deferOne(ctx, v, m); err != nil {
			return err
		}
		n++
	}
	return done(w, r, v, "/unfiled", http.StatusOK, map[string]int{"deferred": n})
}

func (s *Server) matterSearch(w http.ResponseWriter, r *http.Request, v Viewer) error {
	if !v.Staff() {
		return errNotFound
	}
	ms, err := s.Core.ListMatters(r.Context(), v.Auth(), r.URL.Query().Get("q"), 20)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": ms})
	return nil
}
