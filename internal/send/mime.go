// Package send builds and sends mail: quick email, replies and forwards, the add-in's
// sends, system mail and the filing summary. Every send returns a definite sent or
// failed result with the reason, and every attempt is one outbound row.
package send

import (
	"bytes"
	"io"
	"net/mail"
	"strings"
	"time"

	gomail "github.com/emersion/go-message/mail"
)

// Attachment is one file on an outgoing message.
type Attachment struct {
	Name, MediaType string
	Data            []byte
}

// Message is an outgoing message before it is written as MIME.
type Message struct {
	From         string
	To, Cc       []string // Bcc never goes in the headers
	Subject      string
	Text         string
	MessageID    string // with angle brackets
	InReplyTo    string // with angle brackets
	References   []string
	Date         time.Time
	Attachments  []Attachment
	ExtraHeaders map[string]string
}

func bare(id string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
}

func list(addrs []string) []*gomail.Address {
	var out []*gomail.Address
	for _, a := range addrs {
		out = append(out, &gomail.Address{Address: a})
	}
	return out
}

// Build writes m as one raw RFC 5322 message: a text body, plus each attachment.
func Build(m Message) ([]byte, error) {
	var h gomail.Header
	h.SetDate(m.Date)
	h.SetAddressList("From", list([]string{m.From}))
	h.SetAddressList("To", list(m.To))
	if len(m.Cc) > 0 {
		h.SetAddressList("Cc", list(m.Cc))
	}
	h.SetSubject(m.Subject)
	h.SetMessageID(bare(m.MessageID))
	if m.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{bare(m.InReplyTo)})
	}
	if len(m.References) > 0 {
		var refs []string
		for _, r := range m.References {
			refs = append(refs, bare(r))
		}
		h.SetMsgIDList("References", refs)
	}
	for k, v := range m.ExtraHeaders {
		h.Set(k, v)
	}
	var buf bytes.Buffer
	text := func(w io.Writer) error {
		_, err := io.WriteString(w, strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n"))
		return err
	}
	var th gomail.InlineHeader
	th.Set("Content-Type", "text/plain; charset=utf-8")
	if len(m.Attachments) == 0 {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		w, err := gomail.CreateSingleInlineWriter(&buf, h)
		if err != nil {
			return nil, err
		}
		if err := text(w); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	mw, err := gomail.CreateWriter(&buf, h)
	if err != nil {
		return nil, err
	}
	tw, err := mw.CreateSingleInline(th)
	if err != nil {
		return nil, err
	}
	if err := text(tw); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	for _, a := range m.Attachments {
		var ah gomail.AttachmentHeader
		mt := a.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		ah.Set("Content-Type", mt)
		ah.SetFilename(a.Name)
		w, err := mw.CreateAttachment(ah)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(a.Data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ValidAddress reports whether s is one plain address (no display name, no list).
func ValidAddress(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " ,;<>\r\n") {
		return false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return at > zero && strings.Contains(s[at+one:], ".")
}

// Addresses splits a comma- or semicolon-separated list, lower-cased.
func Addresses(v []string) []string {
	var out []string
	for _, s := range v {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
