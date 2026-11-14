// Package filing writes one message to one matter (section 8.4): the HTML copy and each
// real attachment as core files, an attachment link from the copy to each, one note,
// the filings row and the matter's mail extension, then the account's after-filing
// action. Every core write carries an Idempotency-Key built from the message id and the
// step, so a filing stopped halfway resumes without a duplicate file, link or note.
package filing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/mailparse"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/render"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// Errors a caller branches on.
var (
	// ErrNotInMailbox: the raw message is gone; the row offers File without attachments.
	ErrNotInMailbox = errors.New("message_not_in_mailbox")
	// ErrChanged: the mailbox's copy no longer matches the stored SHA-256.
	ErrChanged = errors.New("message_changed_at_provider")
	// ErrDuplicate: the same Message-ID is already filed to the matter.
	ErrDuplicate = errors.New("duplicate")
)

// NotInMailbox is the error text stored on the row.
const NotInMailbox = "message_not_in_mailbox"

// Mailbox opens the provider for an account.
type Mailbox func(ctx context.Context, a store.Account) (provider.Provider, error)

// Filer files messages.
type Filer struct {
	Store   *store.Store
	Core    *coreapi.Client
	Mailbox Mailbox
	// TempDir holds the raw working copy while a filing runs ("" is the OS default).
	TempDir string
}

// Request is one filing.
type Request struct {
	Message  store.Message
	Account  store.Account
	MatterID string
	How      string
	// Auth is the viewer's context token (queue, matter tab, add-in) or the background
	// filing grant.
	Auth         coreapi.Auth
	FiledBy      string // the user, or "" when automatic
	AttorneyOnly bool
	// WithoutAttachments files the stored body only (the raw message is gone).
	WithoutAttachments bool
	// Raw, when set, is the message itself (the add-in's item, a sent copy), so nothing
	// is fetched.
	Raw []byte
	// SkipAfterFile leaves the provider alone (a sent copy built here).
	SkipAfterFile bool
	// Existing maps an attachment's SHA-256 to the matter file it already is (a sent
	// copy's attachments came from the matter): the HTML copy links to that file and
	// nothing is uploaded again.
	Existing map[string]string
}

// File runs the filing. A message already filed returns its filing.
func (f *Filer) File(ctx context.Context, r Request) (store.Filing, error) {
	m := r.Message
	if done, err := f.Store.Filing(ctx, m.ID); err == nil {
		return done, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Filing{}, err
	}
	if other, err := f.Store.MessageByRFC(ctx, m.RFCMessageID); err == nil && other.ID != m.ID && other.Status == "filed" {
		if of, err := f.Store.Filing(ctx, other.ID); err == nil && of.MatterID == r.MatterID {
			_ = f.Store.SetMessageStatus(ctx, m.ID, "duplicate", "duplicate", "")
			return of, ErrDuplicate
		}
	}

	var parsed *mailparse.Message
	var missing []string
	if r.WithoutAttachments {
		parts, err := f.Store.Parts(ctx, m.ID)
		if err != nil {
			return store.Filing{}, err
		}
		for _, p := range parts {
			if !p.Inline {
				missing = append(missing, p.Filename)
			}
		}
	} else {
		raw := r.Raw
		if raw == nil {
			var err error
			if raw, err = f.fetch(ctx, r.Account, m); err != nil {
				if errors.Is(err, ErrNotInMailbox) {
					_ = f.Store.SetMessageStatus(ctx, m.ID, "unfiled", orReason(m.MatchReason), NotInMailbox)
				}
				return store.Filing{}, err
			}
		}
		var err error
		if parsed, err = mailparse.Parse(raw); err != nil {
			return store.Filing{}, err
		}
	}

	// A different kind of filing of the same message gets its own keys, so the core
	// never sees one key with two bodies.
	key := "eml-" + m.ID
	if r.AttorneyOnly {
		key += "-ao"
	}
	if r.WithoutAttachments {
		key += "-body"
	}
	origin := "received"
	tag := "email-in"
	switch m.Direction {
	case "outbound":
		origin, tag = "firm", "email-out"
	case "court":
		tag = "court-notice"
	}

	// The HTML copy: header block over the sanitized body, remote resources stripped,
	// links live.
	date := time.UnixMilli(m.ReceivedAt)
	if m.SentAt != nil {
		date = time.UnixMilli(*m.SentAt)
	}
	h := render.Header{From: m.FromHeader, To: strings.Join(m.ToAddrs, ", "), Cc: strings.Join(m.CcAddrs, ", "), Subject: m.Subject, Date: date}
	body, text := m.BodyHTML, m.BodyText
	if parsed != nil {
		body, text = render.Sanitize(parsed.HTML), parsed.Text
	}
	doc := []byte(render.Document(h, body, text))
	name := render.FileName(date, m.Subject)
	htmlFile, err := f.Core.PublishFile(ctx, r.Auth, key+"-html", r.MatterID, name, "text/html; charset=utf-8", mailparse.Sum(doc), doc, origin, r.AttorneyOnly)
	if err != nil {
		return store.Filing{}, fmt.Errorf("filing the message copy: %w", err)
	}

	// Each real attachment, inline cid images skipped, linked from the copy.
	var names []string
	if parsed != nil {
		for _, p := range parsed.Attachments() {
			afID, ok := r.Existing[p.SHA256]
			if !ok {
				pk := fmt.Sprintf("%s-part-%d", key, p.Index)
				af, err := f.Core.PublishFile(ctx, r.Auth, pk, r.MatterID, p.Filename, p.MediaType, p.SHA256, p.Data, origin, r.AttorneyOnly)
				if err != nil {
					return store.Filing{}, fmt.Errorf("filing attachment %s: %w", p.Filename, err)
				}
				afID = af.ID
			}
			if err := f.Store.SetPartFile(ctx, m.ID, p.Index, afID); err != nil {
				return store.Filing{}, err
			}
			if err := f.Core.LinkAttachment(ctx, r.Auth, fmt.Sprintf("%s-link-%d", key, p.Index), htmlFile.ID, afID); err != nil {
				return store.Filing{}, fmt.Errorf("linking attachment %s: %w", p.Filename, err)
			}
			names = append(names, p.Filename)
		}
	}

	title, noteBody := Note(m, date, text, body, names, missing, name, r.AttorneyOnly)
	note, err := f.Core.CreateNote(ctx, r.Auth, key+"-note", r.MatterID, title, noteBody, []string{"email", tag})
	if err != nil {
		return store.Filing{}, fmt.Errorf("writing the note: %w", err)
	}
	fl := store.Filing{MessageID: m.ID, MatterID: r.MatterID, RFCMessageID: m.RFCMessageID, NoteID: note.ID, HTMLFileID: htmlFile.ID,
		FiledBy: r.FiledBy, How: r.How, AttorneyOnly: r.AttorneyOnly}
	if err := f.Store.RecordFiling(ctx, fl, m.Direction); err != nil {
		return store.Filing{}, err
	}

	// The after-filing action runs only once the note is written.
	if !r.SkipAfterFile && !r.WithoutAttachments && m.ProviderRef != "" && r.Raw == nil {
		if err := f.afterFile(ctx, r.Account, m); err != nil {
			_ = f.Store.SetMessageStatus(ctx, m.ID, "filed", m.MatchReason, "after filing: "+err.Error())
		}
	}
	return f.Store.Filing(ctx, m.ID)
}

func orReason(r string) string {
	if r == "" {
		return NotInMailbox
	}
	return r
}

// fetch reads the raw message into a temporary working copy, checks it against the
// stored SHA-256 and removes the copy when done.
func (f *Filer) fetch(ctx context.Context, a store.Account, m store.Message) ([]byte, error) {
	p, err := f.Mailbox(ctx, a)
	if err != nil {
		return nil, err
	}
	raw, err := p.Fetch(ctx, provider.ParseRef(m.ProviderRef))
	if errors.Is(err, provider.ErrNotFound) {
		return nil, ErrNotInMailbox
	}
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(f.TempDir, "togavsu-email-*.eml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	copyRaw, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	if m.RawSHA256 != "" && mailparse.Sum(copyRaw) != m.RawSHA256 {
		return nil, ErrChanged
	}
	return copyRaw, nil
}

func (f *Filer) afterFile(ctx context.Context, a store.Account, m store.Message) error {
	act := provider.AfterFileAction{Kind: a.AfterFile, MarkRead: a.Role == "court"}
	if act.Kind == "leave" && !act.MarkRead {
		return nil
	}
	p, err := f.Mailbox(ctx, a)
	if err != nil {
		return err
	}
	return p.AfterFile(ctx, provider.ParseRef(m.ProviderRef), act)
}

const (
	titleMax = 200
	textMax  = 20000
)

// Note builds the filing note: title "Email: <subject>"; body the From, To, Cc and Date
// lines, a blank line, the text body, then the attachment names and the full message's
// file name. An attorney-only filing writes only a pointer to the file, because notes
// carry no attorney-only flag.
func Note(m store.Message, date time.Time, text, html string, attachments, missing []string, fileName string, attorneyOnly bool) (string, string) {
	if attorneyOnly {
		return "Email (attorney only)", "Filed as attorney-only; open the file."
	}
	subject := m.Subject
	if strings.TrimSpace(subject) == "" {
		subject = "(no subject)"
	}
	title := cutRunes("Email: "+subject, titleMax)
	if strings.TrimSpace(text) == "" {
		text = render.Text(html)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\n", m.FromHeader)
	fmt.Fprintf(&b, "To: %s\n", strings.Join(m.ToAddrs, ", "))
	if len(m.CcAddrs) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", strings.Join(m.CcAddrs, ", "))
	}
	fmt.Fprintf(&b, "Date: %s\n\n", date.UTC().Format(time.RFC1123Z))
	b.WriteString(cutRunes(strings.TrimSpace(text), textMax))
	b.WriteString("\n\n")
	if len(attachments) > 0 {
		fmt.Fprintf(&b, "Attachments: %s\n", strings.Join(attachments, ", "))
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "Attachments not filed (gone from the mailbox): %s\n", strings.Join(missing, ", "))
	}
	fmt.Fprintf(&b, "Full message: %s", fileName)
	return title, b.String()
}

func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
