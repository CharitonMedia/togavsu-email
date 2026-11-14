// Package imapsmtp is the adapter for any standard IMAP/SMTP mailbox. It reads over
// IMAP (TLS or STARTTLS) with go-imap v2 and sends with internal/smtpsend, then
// APPENDs the sent copy, because SMTP keeps none. Each call opens its own
// connection, bound to the call's context, and logs out when done.
package imapsmtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/smtpsend"
)

// FiledKeyword is the IMAP keyword the label action sets; IMAP has no labels.
const FiledKeyword = imap.Flag("Togavsu-Filed")

// Adapter is one IMAP/SMTP account.
type Adapter struct {
	c provider.Config
	// TLSConfig overrides the TLS settings for both IMAP and SMTP; tests use it to
	// trust a certificate made at runtime.
	TLSConfig *tls.Config
}

// New returns the adapter for c.
func New(c provider.Config) *Adapter { return &Adapter{c: c} }

func (a *Adapter) tlsConfig(host string) *tls.Config {
	if a.TLSConfig != nil {
		c := a.TLSConfig.Clone()
		if c.ServerName == "" {
			c.ServerName = host
		}
		return c
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

func (a *Adapter) accessToken() string {
	if a.c.XOAUTH2 && a.c.Token != nil {
		return a.c.Token.AccessToken
	}
	return ""
}

func (a *Adapter) login() string {
	if a.c.Login != "" {
		return a.c.Login
	}
	return a.c.Address
}

// session is one signed-in connection.
type session struct {
	*imapclient.Client
	stop func() bool
}

func (s *session) close() {
	s.Logout().Wait()
	s.Close()
	s.stop()
}

// dial connects and signs in.
func (a *Adapter) dial(ctx context.Context) (*session, error) {
	addr := net.JoinHostPort(a.c.IMAPHost, strconv.Itoa(a.c.IMAPPort))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	opts := &imapclient.Options{TLSConfig: a.tlsConfig(a.c.IMAPHost)}
	var c *imapclient.Client
	if a.c.IMAPSecurity == "starttls" {
		c, err = imapclient.NewStartTLS(conn, opts)
		if err != nil {
			conn.Close()
			stop()
			return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
		}
	} else {
		c = imapclient.New(tls.Client(conn, opts.TLSConfig), opts)
	}
	s := &session{Client: c, stop: stop}
	if err := c.WaitGreeting(); err != nil {
		c.Close()
		stop()
		return nil, a.wrap(ctx, err)
	}
	if tok := a.accessToken(); tok != "" {
		err = c.Authenticate(xoauth2{a.login(), tok})
	} else {
		err = c.Login(a.login(), a.c.Password).Wait()
	}
	if err != nil {
		c.Close()
		stop()
		var ie *imap.Error
		if errors.As(err, &ie) {
			return nil, provider.ErrAuth
		}
		return nil, a.wrap(ctx, err)
	}
	return s, nil
}

func (a *Adapter) wrap(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ie *imap.Error
	if errors.As(err, &ie) || errors.Is(err, provider.ErrNotFound) || errors.Is(err, provider.ErrAuth) || errors.Is(err, provider.ErrUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
}

// with runs f on a signed-in session.
func (a *Adapter) with(ctx context.Context, f func(*session) error) error {
	s, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	return a.wrap(ctx, f(s))
}

// Check implements provider.Provider: sign in and list.
func (a *Adapter) Check(ctx context.Context) error {
	_, err := a.Folders(ctx)
	return err
}

func special(attrs []imap.MailboxAttr) (string, bool) {
	sp := ""
	for _, at := range attrs {
		switch at {
		case imap.MailboxAttrNoSelect, imap.MailboxAttrNonExistent:
			return "", false
		case imap.MailboxAttrSent, imap.MailboxAttrTrash, imap.MailboxAttrJunk, imap.MailboxAttrDrafts,
			imap.MailboxAttrArchive, imap.MailboxAttrAll, imap.MailboxAttrFlagged:
			sp = string(at)
		}
	}
	return sp, true
}

func list(s *session) ([]provider.Folder, error) {
	data, err := s.List("", "*", nil).Collect()
	if err != nil {
		return nil, err
	}
	var out []provider.Folder
	for _, d := range data {
		sp, ok := special(d.Attrs)
		if !ok {
			continue
		}
		out = append(out, provider.Folder{Ref: d.Mailbox, Name: d.Mailbox, Kind: provider.KindFor(d.Mailbox, sp)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// Folders implements provider.Provider.
func (a *Adapter) Folders(ctx context.Context) ([]provider.Folder, error) {
	var out []provider.Folder
	err := a.with(ctx, func(s *session) (err error) {
		out, err = list(s)
		return err
	})
	return out, err
}

func refID(validity uint32, uid imap.UID) string {
	return strconv.FormatUint(uint64(validity), 10) + "." + strconv.FormatUint(uint64(uid), 10)
}

func parseID(id string) (uint32, imap.UID, bool) {
	v, u, ok := strings.Cut(id, ".")
	vv, err1 := strconv.ParseUint(v, 10, 32)
	uu, err2 := strconv.ParseUint(u, 10, 32)
	return uint32(vv), imap.UID(uu), ok && err1 == nil && err2 == nil && uu > 0
}

// Changes implements provider.Provider: UID SEARCH above the cursor's last UID; a
// changed UIDVALIDITY starts the folder again from since.
func (a *Adapter) Changes(ctx context.Context, f provider.Folder, cur provider.Cursor, since time.Time, limit int) ([]provider.Ref, provider.Cursor, error) {
	var refs []provider.Ref
	next := cur
	err := a.with(ctx, func(s *session) error {
		sel, err := s.Select(f.Ref, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return err
		}
		last := imap.UID(cur.LastUID)
		if sel.UIDValidity != cur.UIDValidity {
			last = 0
		}
		next = provider.Cursor{UIDValidity: sel.UIDValidity, LastUID: uint32(last)}
		if sel.NumMessages == 0 {
			return nil
		}
		crit := &imap.SearchCriteria{UID: []imap.UIDSet{{imap.UIDRange{Start: last + 1, Stop: 0}}}}
		if !since.IsZero() {
			crit.Since = since
		}
		res, err := s.UIDSearch(crit, nil).Wait()
		if err != nil {
			return err
		}
		var uids []imap.UID
		for _, u := range res.AllUIDs() {
			// "n:*" always matches the highest UID, even when it is below n.
			if u > last {
				uids = append(uids, u)
			}
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		if limit > 0 && len(uids) > limit {
			uids = uids[:limit]
		}
		for _, u := range uids {
			refs = append(refs, provider.Ref{Folder: f.Ref, ID: refID(sel.UIDValidity, u)})
			next.LastUID = uint32(u)
		}
		return nil
	})
	return refs, next, err
}

// selectRef selects the ref's folder and checks its UIDVALIDITY still matches.
func selectRef(s *session, r provider.Ref, readOnly bool) (imap.UID, error) {
	v, uid, ok := parseID(r.ID)
	if !ok {
		return 0, provider.ErrNotFound
	}
	sel, err := s.Select(r.Folder, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) {
			return 0, provider.ErrNotFound
		}
		return 0, err
	}
	if sel.UIDValidity != v {
		return 0, provider.ErrNotFound
	}
	return uid, nil
}

func fetchRaw(s *session, uid imap.UID) ([]byte, error) {
	sec := &imap.FetchItemBodySection{Peek: true}
	msgs, err := s.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.UID == uid {
			if b := m.FindBodySection(sec); b != nil {
				return b, nil
			}
		}
	}
	return nil, provider.ErrNotFound
}

// Fetch implements provider.Provider with UID FETCH BODY.PEEK[], so reading never
// sets \Seen.
func (a *Adapter) Fetch(ctx context.Context, r provider.Ref) ([]byte, error) {
	var raw []byte
	err := a.with(ctx, func(s *session) error {
		uid, err := selectRef(s, r, true)
		if err != nil {
			return err
		}
		raw, err = fetchRaw(s, uid)
		return err
	})
	return raw, err
}

func store(s *session, uid imap.UID, op imap.StoreFlagsOp, flags ...imap.Flag) error {
	return s.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: op, Silent: true, Flags: flags}, nil).Close()
}

func expunge(s *session, uid imap.UID) error {
	if err := store(s, uid, imap.StoreFlagsAdd, imap.FlagDeleted); err != nil {
		return err
	}
	if s.Caps().Has(imap.CapUIDPlus) || s.Caps().Has(imap.CapIMAP4rev2) {
		return s.UIDExpunge(imap.UIDSetNum(uid)).Close()
	}
	return s.Expunge().Close()
}

// AfterFile implements provider.Provider: label sets the Togavsu-Filed keyword, move
// goes to Filed, delete is STORE \Deleted and UID EXPUNGE.
func (a *Adapter) AfterFile(ctx context.Context, r provider.Ref, act provider.AfterFileAction) error {
	if act.Kind == "move" {
		if act.MarkRead {
			if err := a.MarkRead(ctx, r, true); err != nil {
				return err
			}
		}
		_, err := a.Move(ctx, r, provider.FiledFolder)
		return err
	}
	return a.with(ctx, func(s *session) error {
		uid, err := selectRef(s, r, false)
		if err != nil {
			return err
		}
		if act.MarkRead {
			if err := store(s, uid, imap.StoreFlagsAdd, imap.FlagSeen); err != nil {
				return err
			}
		}
		switch act.Kind {
		case "", "leave":
			return nil
		case "label":
			return store(s, uid, imap.StoreFlagsAdd, FiledKeyword)
		case "delete":
			return expunge(s, uid)
		}
		return fmt.Errorf("imapsmtp: unknown after-filing action %q", act.Kind)
	})
}

func ensure(s *session, folder string) error {
	err := s.Create(folder, nil).Wait()
	var ie *imap.Error
	if err == nil || (errors.As(err, &ie) && (ie.Code == imap.ResponseCodeAlreadyExists || ie.Type == imap.StatusResponseTypeNo)) {
		return nil
	}
	return err
}

func messageID(raw []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(m.Header.Get("Message-Id"))
}

// Move implements provider.Provider with MOVE, or COPY, STORE \Deleted and EXPUNGE on
// a server without it. The new UID comes from COPYUID, or from a search of the folder
// by Message-ID when the server gives none.
func (a *Adapter) Move(ctx context.Context, r provider.Ref, folder string) (provider.Ref, error) {
	var out provider.Ref
	err := a.with(ctx, func(s *session) error {
		if err := ensure(s, folder); err != nil {
			return err
		}
		uid, err := selectRef(s, r, false)
		if err != nil {
			return err
		}
		var msgID string
		if b, err := s.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true,
			BodySection: []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierHeader, Peek: true}}}).Collect(); err == nil && len(b) > 0 {
			for _, sec := range b[0].BodySection {
				msgID = messageID(append(sec.Bytes, '\r', '\n'))
			}
		}
		md, err := s.Move(imap.UIDSetNum(uid), folder).Wait()
		if err != nil {
			return err
		}
		if md != nil && md.UIDValidity != 0 {
			if dest, ok := md.DestUIDs.(imap.UIDSet); ok {
				if nums, ok := dest.Nums(); ok && len(nums) == 1 {
					out = provider.Ref{Folder: folder, ID: refID(md.UIDValidity, nums[0])}
					return nil
				}
			}
		}
		sel, err := s.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return err
		}
		crit := &imap.SearchCriteria{}
		if msgID != "" {
			crit.Header = []imap.SearchCriteriaHeaderField{{Key: "Message-Id", Value: msgID}}
		}
		res, err := s.UIDSearch(crit, nil).Wait()
		if err != nil {
			return err
		}
		uids := res.AllUIDs()
		if len(uids) == 0 {
			return provider.ErrNotFound
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		out = provider.Ref{Folder: folder, ID: refID(sel.UIDValidity, uids[len(uids)-1])}
		return nil
	})
	return out, err
}

// MarkRead implements provider.Provider.
func (a *Adapter) MarkRead(ctx context.Context, r provider.Ref, read bool) error {
	return a.with(ctx, func(s *session) error {
		uid, err := selectRef(s, r, false)
		if err != nil {
			return err
		}
		op := imap.StoreFlagsAdd
		if !read {
			op = imap.StoreFlagsDel
		}
		return store(s, uid, op, imap.FlagSeen)
	})
}

// Send implements provider.Provider: SMTP, then APPEND to the Sent folder. The answer
// is the message's Message-ID. A sent message whose copy can't be appended is still
// sent, so that failure is not returned.
func (a *Adapter) Send(ctx context.Context, raw []byte, env provider.Envelope) (string, error) {
	srv := smtpsend.Server{Host: a.c.SMTPHost, Port: a.c.SMTPPort, Login: a.login(), Password: a.c.Password,
		Token: a.accessToken(), Implicit: a.c.SMTPSecurity == "tls", TLSConfig: a.TLSConfig}
	from := env.From
	if from == "" {
		from = a.c.Address
	}
	if err := smtpsend.Send(ctx, srv, from, env.To, raw); err != nil {
		if errors.Is(err, smtpsend.ErrAuth) {
			return "", provider.ErrAuth
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	_ = a.with(ctx, func(s *session) error {
		fs, err := list(s)
		if err != nil {
			return err
		}
		sent := ""
		for _, f := range fs {
			if f.Kind == provider.KindSent {
				sent = f.Ref
				break
			}
		}
		if sent == "" {
			sent = "Sent"
			if err := ensure(s, sent); err != nil {
				return err
			}
		}
		ap := s.Append(sent, int64(len(raw)), &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen}, Time: time.Now()})
		if _, err := ap.Write(raw); err != nil {
			return err
		}
		if err := ap.Close(); err != nil {
			return err
		}
		_, err = ap.Wait()
		return err
	})
	return messageID(raw), nil
}

// xoauth2 is Google's and Microsoft's XOAUTH2 SASL mechanism for IMAP.
type xoauth2 struct{ user, token string }

var _ sasl.Client = xoauth2{}

func (x xoauth2) Start() (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + x.user + "\x01auth=Bearer " + x.token + "\x01\x01"), nil
}

func (x xoauth2) Next([]byte) ([]byte, error) { return []byte{}, nil }
