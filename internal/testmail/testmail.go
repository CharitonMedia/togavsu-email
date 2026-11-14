// Package testmail builds synthetic RFC 5322 messages for tests. Every address and name
// in it is made up (example.test domains).
package testmail

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Attachment is one file in a test message.
type Attachment struct {
	Name, Type string
	Data       []byte
	// ContentID makes it an inline cid image.
	ContentID string
}

// Msg describes a test message.
type Msg struct {
	ID          string // without brackets; empty leaves the header out
	From        string
	To          []string
	Cc          []string
	DeliveredTo string
	Subject     string
	Date        time.Time
	Text        string
	HTML        string
	InReplyTo   string // with brackets
	References  []string
	Attachments []Attachment
}

// Raw writes the message.
func (m Msg) Raw() []byte {
	var b strings.Builder
	h := func(k, v string) {
		if v != "" {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	if m.Date.IsZero() {
		m.Date = time.Date(2026, 11, 2, 15, 4, 5, 0, time.UTC)
	}
	h("Delivered-To", m.DeliveredTo)
	h("From", m.From)
	h("To", strings.Join(m.To, ", "))
	h("Cc", strings.Join(m.Cc, ", "))
	h("Subject", m.Subject)
	h("Date", m.Date.Format(time.RFC1123Z))
	if m.ID != "" {
		h("Message-ID", "<"+m.ID+">")
	}
	h("In-Reply-To", m.InReplyTo)
	h("References", strings.Join(m.References, " "))
	h("MIME-Version", "1.0")
	text := m.Text
	if text == "" && m.HTML == "" {
		text = "(no text)"
	}
	if len(m.Attachments) == 0 && m.HTML == "" {
		h("Content-Type", "text/plain; charset=utf-8")
		b.WriteString("\r\n" + text + "\r\n")
		return []byte(b.String())
	}
	h("Content-Type", `multipart/mixed; boundary="mix-1"`)
	b.WriteString("\r\n--mix-1\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"alt-1\"\r\n\r\n")
	b.WriteString("--alt-1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + text + "\r\n")
	if m.HTML != "" {
		b.WriteString("--alt-1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + m.HTML + "\r\n")
	}
	b.WriteString("--alt-1--\r\n")
	for _, a := range m.Attachments {
		b.WriteString("--mix-1\r\n")
		typ := a.Type
		if typ == "" {
			typ = "application/octet-stream"
		}
		b.WriteString("Content-Type: " + typ + "\r\nContent-Transfer-Encoding: base64\r\n")
		if a.ContentID != "" {
			b.WriteString("Content-ID: <" + a.ContentID + ">\r\nContent-Disposition: inline; filename=\"" + a.Name + "\"\r\n\r\n")
		} else {
			b.WriteString("Content-Disposition: attachment; filename=\"" + a.Name + "\"\r\n\r\n")
		}
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	b.WriteString("--mix-1--\r\n")
	return []byte(b.String())
}

// PDF returns small synthetic bytes named n.
func PDF(n int) []byte { return []byte(fmt.Sprintf("%%PDF-1.4\n%% synthetic test document %d\n%%%%EOF\n", n)) }

// PNG is a tiny synthetic image body.
var PNG = []byte("\x89PNG\r\n\x1a\nsynthetic")
