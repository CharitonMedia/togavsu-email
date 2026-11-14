// Package provider is the one interface sync, filing and send use for every mail
// provider: Gmail (provider/gmail), Microsoft Graph (provider/graph), IMAP/SMTP
// (provider/imapsmtp) and the in-memory fake every test uses (provider/fake).
package provider

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Folder kinds, as stored in mod_email.folders.kind.
const (
	KindInbox       = "inbox"
	KindSent        = "sent"
	KindOther       = "other"
	KindSkip        = "skip"
	KindFiled       = "filed"
	KindReviewLater = "review_later"
)

// Folder names the module creates when missing.
const (
	FiledFolder       = "Filed"
	ReviewLaterFolder = "Review-Later"
	// GmailFiledLabel is the label Gmail accounts get after filing.
	GmailFiledLabel = "Togavsu/Filed"
)

// Errors every adapter maps its failures to.
var (
	// ErrAuth is a sign-in failure: the account needs to be connected again.
	ErrAuth = errors.New("provider: sign-in failed")
	// ErrNotFound is a message that is no longer in the mailbox.
	ErrNotFound = errors.New("provider: message not in mailbox")
	// ErrUnavailable is a provider that can't be reached right now.
	ErrUnavailable = errors.New("provider: unavailable")
)

// Folder is one folder or label.
type Folder struct {
	Ref         string
	Name        string
	Kind        string
	UIDValidity uint32
}

// Cursor is where a folder's sync stands: the IMAP UIDVALIDITY and last UID, or the
// Gmail history id or Graph delta link in Token.
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
	Token       string
}

// Ref names one message at the provider.
type Ref struct {
	Folder string
	ID     string
}

// String is the provider_ref stored on the message row.
func (r Ref) String() string { return url.PathEscape(r.Folder) + "/" + url.PathEscape(r.ID) }

// ParseRef reads a stored provider_ref.
func ParseRef(s string) Ref {
	f, id, _ := strings.Cut(s, "/")
	f, _ = url.PathUnescape(f)
	id, _ = url.PathUnescape(id)
	return Ref{Folder: f, ID: id}
}

// AfterFileAction is what happens at the provider after a clean filing.
type AfterFileAction struct {
	// Kind is leave, label, move or delete.
	Kind string
	// MarkRead marks the message read first (court accounts).
	MarkRead bool
}

// Envelope is the SMTP envelope of a send: the sender and every recipient, Bcc included.
type Envelope struct {
	From string
	To   []string
}

// Provider is one connected mailbox.
type Provider interface {
	Check(ctx context.Context) error
	Folders(ctx context.Context) ([]Folder, error)
	Changes(ctx context.Context, f Folder, cur Cursor, since time.Time, limit int) ([]Ref, Cursor, error)
	Fetch(ctx context.Context, r Ref) ([]byte, error)
	AfterFile(ctx context.Context, r Ref, a AfterFileAction) error
	Move(ctx context.Context, r Ref, folder string) error
	MarkRead(ctx context.Context, r Ref, read bool) error
	Send(ctx context.Context, raw []byte, env Envelope) (string, error)
}

// Token is an OAuth token set, kept in the core's module secret store.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// Config is what an adapter needs to reach one account. Credentials come from the
// core's secret store at runtime and live only in memory.
type Config struct {
	AccountID    string
	Provider     string // gmail, microsoft, imap
	Address      string
	IMAPHost     string
	IMAPPort     int
	IMAPSecurity string
	SMTPHost     string
	SMTPPort     int
	SMTPSecurity string
	Login        string
	Password     string
	Token        *Token
	// SaveToken stores a refreshed token set.
	SaveToken func(ctx context.Context, t Token) error
	// OAuth client for refreshing Token (the admin's settings).
	ClientID, ClientSecret, Tenant string
	// Endpoint overrides; tests point these at httptest servers on 127.0.0.1.
	TokenURL, APIBase string
	// XOAUTH2 signs IMAP and SMTP in with Token instead of Password.
	XOAUTH2 bool
}

// skipNames is Unified's folder skip list.
var skipNames = map[string]string{
	"archive": KindSkip, "trash": KindSkip, "deleted items": KindSkip, "deleted": KindSkip, "drafts": KindSkip,
	"junk": KindSkip, "junk email": KindSkip, "spam": KindSkip, "bin": KindSkip, "[gmail]/trash": KindSkip,
	"[gmail]/spam": KindSkip, "[gmail]/drafts": KindSkip, "[gmail]/all mail": KindSkip, "[gmail]/important": KindSkip,
	"[gmail]/starred": KindSkip, "outbox": KindSkip, "conversation history": KindSkip,
	"sent": KindSent, "sent items": KindSent, "sent mail": KindSent, "sent messages": KindSent, "[gmail]/sent mail": KindSent,
	"filed": KindFiled, "togavsu/filed": KindFiled, "review-later": KindReviewLater,
	"inbox": KindInbox,
}

// KindFor classifies a folder by its name and, when the server gives one, its special
// use attribute (\Sent, \Trash, \Junk, \Drafts, \Archive, \All).
func KindFor(name, special string) string {
	switch strings.ToLower(special) {
	case `\sent`:
		return KindSent
	case `\trash`, `\junk`, `\drafts`, `\archive`, `\all`, `\flagged`, `\important`:
		return KindSkip
	}
	n := strings.ToLower(strings.TrimSpace(name))
	if k, ok := skipNames[n]; ok {
		return k
	}
	// A subfolder of a skipped folder is skipped too.
	for _, sep := range []string{"/", "."} {
		if i := strings.Index(n, sep); i > 0 {
			if k, ok := skipNames[n[:i]]; ok && k != KindInbox {
				if k == KindSent {
					return KindSkip
				}
				return k
			}
		}
	}
	return KindOther
}
