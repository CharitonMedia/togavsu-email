package send

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/filing"
	"github.com/CharitonMedia/togavsu-email/internal/ids"
	"github.com/CharitonMedia/togavsu-email/internal/mailparse"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// MaxAttachments is the most a compose may attach in all.
const MaxAttachments = 20 << 20

// Errors the HTTP layer maps to status codes.
var (
	ErrInvalid       = errors.New("invalid")        // 400 (422 for system mail)
	ErrNotFound      = errors.New("not_found")      // 404
	ErrNoAccount     = errors.New("no_account")     // 409: nothing to send from
	ErrFailed        = errors.New("send_failed")    // 502
	ErrNotConfigured = errors.New("not_configured") // 503, system mail
)

// Invalid is a refused compose with its reason.
type Invalid struct{ Field, Reason string }

func (e *Invalid) Error() string { return e.Field + ": " + e.Reason }
func (e *Invalid) Unwrap() error { return ErrInvalid }

// Sender sends.
type Sender struct {
	Store    *store.Store
	Core     *coreapi.Client
	Open     filing.Mailbox
	Filer    *filing.Filer
	Settings func(ctx context.Context) (config.Settings, error)
	Now      func() time.Time
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Compose is one person's send.
type Compose struct {
	Kind           string // quick, reply, forward, addin
	AccountID      string
	To, Cc, Bcc    []string
	Subject        string
	Body           string
	MatterID       string
	FileIDs        []string
	ReplyTo        string // the stored message being answered or forwarded
	IdempotencyKey string
}

// Result is a send's outcome.
type Result struct {
	Status      string `json:"status"`
	Recorded    bool   `json:"recorded"`
	OutboundID  string `json:"outbound_id"`
	ProviderRef string `json:"provider_message_id"`
	MessageID   string `json:"message_id"`
	AccountID   string `json:"account_id"`
	Error       string `json:"error,omitempty"`
}

// Viewer is the person sending.
type Viewer struct {
	coreapi.ViewerInfo
	Token string
}

// MaySendAs reports whether v may send as account a: a personal account's owner, or
// staff on a firm account that sends.
func MaySendAs(v coreapi.ViewerInfo, a store.Account) bool {
	if !a.CanSend || a.Status == "removed" || a.Role == "court" {
		return false
	}
	if a.Role == "personal" {
		return a.OwnerUserID == v.UserID
	}
	return v.Staff()
}

// pickAccount follows section 8.6: the account the answered message came in on when the
// viewer may send as it, else their own personal account, else the default_send one.
func (s *Sender) pickAccount(ctx context.Context, v coreapi.ViewerInfo, want string, original *store.Message) (store.Account, error) {
	if want != "" {
		a, err := s.Store.Account(ctx, want)
		if err != nil || !MaySendAs(v, a) {
			return store.Account{}, ErrNotFound
		}
		return a, nil
	}
	if original != nil {
		if a, err := s.Store.Account(ctx, original.AccountID); err == nil && MaySendAs(v, a) {
			return a, nil
		}
	}
	own, err := s.Store.PersonalAccounts(ctx, v.UserID)
	if err != nil {
		return store.Account{}, err
	}
	for _, a := range own {
		if MaySendAs(v, a) && a.Status != "paused" {
			return a, nil
		}
	}
	a, err := s.Store.DefaultSendAccount(ctx)
	if err == nil && MaySendAs(v, a) {
		return a, nil
	}
	return store.Account{}, ErrNoAccount
}

func newMessageID(addr string) string {
	domain := "togavsu.invalid"
	if i := strings.LastIndexByte(addr, '@'); i >= zero {
		domain = addr[i+1:]
	}
	return "<" + ids.New(time.Now()) + "@" + domain + ">"
}

func validList(field string, l []string, required bool) error {
	if required && len(l) == 0 {
		return &Invalid{field, "at least one address is required"}
	}
	for _, a := range l {
		if !ValidAddress(a) {
			return &Invalid{field, "not a valid address: " + a}
		}
	}
	return nil
}

// Send validates, sends and, with a matter, files the sent copy. Nothing is sent when
// the To list or the body is invalid, or the matter or a file is out of the viewer's
// reach.
func (s *Sender) Send(ctx context.Context, v Viewer, c Compose) (Result, error) {
	c.To, c.Cc, c.Bcc = Addresses(c.To), Addresses(c.Cc), Addresses(c.Bcc)
	if err := validList("to", c.To, true); err != nil {
		return Result{}, err
	}
	if err := validList("cc", c.Cc, false); err != nil {
		return Result{}, err
	}
	if err := validList("bcc", c.Bcc, false); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(c.Body) == "" {
		return Result{}, &Invalid{"body", "the message is empty"}
	}
	if len(c.Subject) > subjectMax || strings.ContainsAny(c.Subject, "\r\n") {
		return Result{}, &Invalid{"subject", "one line of at most 998 characters"}
	}
	auth := coreapi.Viewer(v.Token)

	var original *store.Message
	var refs []string
	if c.ReplyTo != "" {
		m, err := s.Store.Message(ctx, c.ReplyTo)
		if err != nil {
			return Result{}, ErrNotFound
		}
		if err := s.mayRead(ctx, v, m); err != nil {
			return Result{}, err
		}
		original = &m
		refs = append(append(refs, m.ReferenceIDs...), m.RFCMessageID)
		if c.MatterID == "" {
			if f, err := s.Store.Filing(ctx, m.ID); err == nil {
				c.MatterID = f.MatterID
			}
		}
	}
	if c.MatterID != "" {
		if _, err := s.Core.GetMatter(ctx, auth, c.MatterID); err != nil {
			if coreapi.IsNotFound(err) || coreapi.StatusOf(err) == http.StatusForbidden {
				return Result{}, ErrNotFound
			}
			return Result{}, err
		}
	}
	var atts []Attachment
	type fileVer struct{ file, version string }
	inMatter := map[string]string{}
	var used []fileVer
	total := zero
	for _, id := range c.FileIDs {
		f, err := s.Core.GetFile(ctx, auth, id)
		if err != nil {
			if coreapi.IsNotFound(err) || coreapi.StatusOf(err) == http.StatusForbidden {
				return Result{}, ErrNotFound
			}
			return Result{}, err
		}
		if c.MatterID == "" || f.MatterID == nil || *f.MatterID != c.MatterID || f.CurrentVersionID == nil {
			return Result{}, &Invalid{"files", "attach files from the chosen matter"}
		}
		vs, err := s.Core.Versions(ctx, auth, id)
		if err != nil {
			return Result{}, err
		}
		mt := "application/octet-stream"
		for _, ver := range vs {
			if ver.ID == *f.CurrentVersionID {
				mt = ver.MediaType
				total += int(ver.SizeBytes)
				inMatter[ver.SHA256] = id
			}
		}
		if total > MaxAttachments {
			return Result{}, &Invalid{"files", "attachments total more than 20 MB"}
		}
		data, err := s.Core.Download(ctx, auth, id, *f.CurrentVersionID)
		if err != nil {
			return Result{}, err
		}
		atts = append(atts, Attachment{Name: f.Name, MediaType: mt, Data: data})
		used = append(used, fileVer{id, *f.CurrentVersionID})
	}
	acct, err := s.pickAccount(ctx, v.ViewerInfo, c.AccountID, original)
	if err != nil {
		return Result{}, err
	}
	kind := c.Kind
	if kind == "" {
		kind = "quick"
	}
	key := c.IdempotencyKey
	if key == "" {
		key = "compose-" + ids.Secret(composeKeyBytes)
	}
	msg := Message{From: acct.Address, To: c.To, Cc: c.Cc, Subject: c.Subject, Text: c.Body, MessageID: newMessageID(acct.Address), Date: s.now(), Attachments: atts}
	if original != nil && kind == "reply" {
		msg.InReplyTo, msg.References = original.RFCMessageID, refs
	}
	raw, err := Build(msg)
	if err != nil {
		return Result{}, err
	}
	o := store.Outbound{AccountID: acct.ID, Kind: kind, IdempotencyKey: v.UserID + ":" + key, To: c.To, Cc: c.Cc, Bcc: c.Bcc, Subject: c.Subject,
		BodyText: c.Body, BodySHA256: mailparse.Sum([]byte(c.Body)), InReplyTo: msg.InReplyTo, MatterID: c.MatterID, CreatedBy: v.UserID}
	existing, err := s.Store.BeginOutbound(ctx, &o)
	if err != nil {
		return Result{}, err
	}
	if existing && o.Status != "queued" && o.Status != "failed" {
		return Result{Status: "sent", Recorded: o.Status == "recorded", OutboundID: o.ID, ProviderRef: o.ProviderRef, AccountID: o.AccountID}, nil
	}
	for _, u := range used {
		if err := s.Store.AddOutboundFile(ctx, o.ID, u.file, u.version); err != nil {
			return Result{}, err
		}
	}
	ref, err := s.deliver(ctx, acct, raw, provider.Envelope{From: acct.Address, To: append(append(append([]string{}, c.To...), c.Cc...), c.Bcc...)})
	if err != nil {
		_ = s.Store.FinishOutbound(ctx, o.ID, "failed", "", err.Error(), false)
		return Result{Status: "failed", OutboundID: o.ID, Error: err.Error()}, fmt.Errorf("%w: %v", ErrFailed, err)
	}
	if err := s.Store.FinishOutbound(ctx, o.ID, "sent", ref, "", false); err != nil {
		return Result{}, err
	}
	res := Result{Status: "sent", OutboundID: o.ID, ProviderRef: ref, MessageID: msg.MessageID, AccountID: acct.ID}
	if c.MatterID == "" {
		return res, nil
	}
	// The record is written only after a confirmed send.
	if err := s.fileSent(ctx, v, acct, o, raw, c.MatterID, inMatter); err != nil {
		_ = s.Store.SetOutboundStatus(ctx, o.ID, "record_failed")
		return res, nil
	}
	_ = s.Store.SetOutboundStatus(ctx, o.ID, "recorded")
	res.Recorded = true
	return res, nil
}

// deliver sends raw, or records it when sending_disabled is on.
func (s *Sender) deliver(ctx context.Context, a store.Account, raw []byte, env provider.Envelope) (string, error) {
	if s.Settings != nil {
		st, err := s.Settings(ctx)
		if err != nil {
			return "", err
		}
		if st.SendingDisabled {
			return "disabled", nil
		}
	}
	p, err := s.Open(ctx, a)
	if err != nil {
		return "", err
	}
	return p.Send(ctx, raw, env)
}

// fileSent stores the sent copy as an outbound mail record and files it to the matter
// as the sender. When the filing fails the copy waits in the queue.
func (s *Sender) fileSent(ctx context.Context, v Viewer, a store.Account, o store.Outbound, raw []byte, matterID string, existing map[string]string) error {
	pm, err := mailparse.Parse(raw)
	if err != nil {
		return err
	}
	m := RecordFromParsed(pm, a.ID, "sent:"+o.ID, "outbound")
	if _, err := s.Store.Claim(ctx, &m); err != nil {
		return err
	}
	if err := s.Store.SetParts(ctx, m.ID, PartsOf(pm)); err != nil {
		return err
	}
	stored, err := s.Store.Message(ctx, m.ID)
	if err != nil {
		return err
	}
	_, err = s.Filer.File(ctx, filing.Request{Message: stored, Account: a, MatterID: matterID, How: "sent", Auth: coreapi.Viewer(v.Token),
		FiledBy: v.UserID, Raw: raw, SkipAfterFile: true, Existing: existing})
	if err != nil && !errors.Is(err, filing.ErrDuplicate) {
		_ = s.Store.SetMatches(ctx, m.ID, []store.Match{{MatterID: matterID, Via: "thread"}})
		_ = s.Store.SetMessageStatus(ctx, m.ID, "unfiled", "sent_copy_not_filed", err.Error())
		return err
	}
	return nil
}

// mayRead applies the queue's visibility to a stored message: personal mail is the
// owner's alone; firm mail is for staff.
func (s *Sender) mayRead(_ context.Context, v Viewer, m store.Message) error {
	if m.Role == "personal" {
		if m.OwnerUserID != v.UserID {
			return ErrNotFound
		}
		return nil
	}
	if !v.Staff() {
		return ErrNotFound
	}
	return nil
}

// RecordFromParsed turns a parsed message into a mail record.
func RecordFromParsed(pm *mailparse.Message, accountID, ref, direction string) store.Message {
	m := store.Message{AccountID: accountID, ProviderRef: ref, RFCMessageID: pm.MessageID, InReplyTo: pm.InReplyTo, ReferenceIDs: pm.References,
		Direction: direction, FromHeader: pm.FromHeader, ToAddrs: pm.To, CcAddrs: pm.Cc, Subject: pm.Subject, Snippet: mailparse.Snippet(pm.Text),
		BodyText: pm.Text, RawSHA256: pm.RawSHA256, RawSize: pm.RawSize, ThreadKey: ThreadKey(pm)}
	if len(pm.From) > 0 {
		m.FromAddr = pm.From[0]
	}
	if len(pm.DeliveredTo) > 0 {
		m.DeliveredTo = pm.DeliveredTo[0]
	}
	if !pm.Date.IsZero() {
		t := pm.Date.UnixMilli()
		m.SentAt = &t
	}
	return m
}

// ThreadKey is the thread's first Message-ID: the first Reference, else In-Reply-To,
// else the message's own.
func ThreadKey(pm *mailparse.Message) string {
	if len(pm.References) > 0 {
		return pm.References[0]
	}
	if pm.InReplyTo != "" {
		return pm.InReplyTo
	}
	return pm.MessageID
}

// PartsOf lists a parsed message's parts for the store.
func PartsOf(pm *mailparse.Message) []store.Part {
	var out []store.Part
	for _, p := range pm.Parts {
		out = append(out, store.Part{Index: p.Index, Filename: p.Filename, MediaType: p.MediaType, Size: p.Size, SHA256: p.SHA256, Inline: p.Inline, ContentID: p.ContentID})
	}
	return out
}

// SystemMail is the core's CC-1 body.
type SystemMail struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	To          string `json:"to"`
	Subject     string `json:"subject"`
	Text        string `json:"text"`
	Attachments []struct {
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		DataBase64  string `json:"data_base64"`
	} `json:"attachments"`
}

// System sends one system mail through the account marked system_mail. A repeated id
// returns the first answer and sends nothing; the body is cleared once sent, because
// system mail carries password reset links.
func (s *Sender) System(ctx context.Context, sm SystemMail) (Result, error) {
	if strings.TrimSpace(sm.ID) == "" {
		return Result{}, &Invalid{"id", "required"}
	}
	to := Addresses([]string{sm.To})
	if err := validList("to", to, true); err != nil {
		return Result{}, err
	}
	if strings.ContainsAny(sm.Subject, "\r\n") {
		return Result{}, &Invalid{"subject", "one line"}
	}
	var atts []Attachment
	for _, a := range sm.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.DataBase64)
		if err != nil {
			return Result{}, &Invalid{"attachments", "data_base64 is not base64"}
		}
		atts = append(atts, Attachment{Name: a.Name, MediaType: a.ContentType, Data: data})
	}
	if prev, err := s.Store.OutboundByKey(ctx, "system:"+sm.ID); err == nil && (prev.Status == "sent" || prev.Status == "recorded") {
		return Result{Status: "sent", OutboundID: prev.ID, ProviderRef: prev.ProviderRef, AccountID: prev.AccountID}, nil
	}
	acct, err := s.Store.SystemMailAccount(ctx)
	if err != nil || acct.Status != "connected" || !acct.CanSend {
		return Result{}, ErrNotConfigured
	}
	o := store.Outbound{AccountID: acct.ID, Kind: "system", IdempotencyKey: "system:" + sm.ID, To: to, Subject: sm.Subject, BodyText: sm.Text,
		BodySHA256: mailparse.Sum([]byte(sm.Text)), SystemKind: sm.Kind}
	if _, err := s.Store.BeginOutbound(ctx, &o); err != nil {
		return Result{}, err
	}
	raw, err := Build(Message{From: acct.Address, To: to, Subject: sm.Subject, Text: sm.Text, MessageID: newMessageID(acct.Address), Date: s.now(), Attachments: atts})
	if err != nil {
		return Result{}, err
	}
	ref, err := s.deliver(ctx, acct, raw, provider.Envelope{From: acct.Address, To: to})
	if err != nil {
		_ = s.Store.FinishOutbound(ctx, o.ID, "failed", "", err.Error(), false)
		return Result{Status: "failed", OutboundID: o.ID, Error: err.Error()}, fmt.Errorf("%w: %v", ErrFailed, err)
	}
	if err := s.Store.FinishOutbound(ctx, o.ID, "sent", ref, "", true); err != nil {
		return Result{}, err
	}
	return Result{Status: "sent", OutboundID: o.ID, ProviderRef: ref, AccountID: acct.ID}, nil
}

// Plain sends a module-written message (the filing summary) from the default send
// account.
func (s *Sender) Plain(ctx context.Context, kind string, to []string, subject, text string) error {
	acct, err := s.Store.DefaultSendAccount(ctx)
	if err != nil || acct.Status != "connected" || !acct.CanSend {
		return ErrNoAccount
	}
	to = Addresses(to)
	if err := validList("to", to, true); err != nil {
		return err
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	o := store.Outbound{AccountID: acct.ID, Kind: "system", IdempotencyKey: kind + ":" + hex.EncodeToString(b[:]), To: to, Subject: subject, BodyText: text,
		BodySHA256: mailparse.Sum([]byte(text)), SystemKind: kind}
	if _, err := s.Store.BeginOutbound(ctx, &o); err != nil {
		return err
	}
	raw, err := Build(Message{From: acct.Address, To: to, Subject: subject, Text: text, MessageID: newMessageID(acct.Address), Date: s.now()})
	if err != nil {
		return err
	}
	ref, err := s.deliver(ctx, acct, raw, provider.Envelope{From: acct.Address, To: to})
	if err != nil {
		_ = s.Store.FinishOutbound(ctx, o.ID, "failed", "", err.Error(), false)
		return err
	}
	return s.Store.FinishOutbound(ctx, o.ID, "sent", ref, "", false)
}
