// Package mailparse reads one raw RFC 5322 message into the facts the module stores and
// files. Every provider fetches the full raw message, so this one parser handles every
// provider's mail the same way.
package mailparse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset" // non-UTF-8 charsets
	"github.com/emersion/go-message/mail"
)

// MaxBytes is the largest message the module reads.
const MaxBytes = 50 << 20

// ErrTooLarge refuses a message over MaxBytes.
var ErrTooLarge = errors.New("message_too_large")

// ErrMalformed is a message the parser can't read.
var ErrMalformed = errors.New("message_malformed")

// Part is one attachment or inline image.
type Part struct {
	Index     int
	Filename  string
	MediaType string
	Size      int64
	SHA256    string
	Inline    bool
	ContentID string
	Data      []byte
}

// Message is a parsed message.
type Message struct {
	MessageID   string // with angle brackets
	InReplyTo   string
	References  []string
	FromHeader  string
	From        []string // lower-case addresses
	To          []string
	Cc          []string
	DeliveredTo []string
	Subject     string
	Date        time.Time
	Text        string
	HTML        string // as sent; render.Sanitize cleans it
	Parts       []Part
	RawSHA256   string
	RawSize     int64
}

// Attachments returns the real attachments: every part but inline cid images.
func (m *Message) Attachments() []Part {
	var out []Part
	for _, p := range m.Parts {
		if !p.Inline {
			out = append(out, p)
		}
	}
	return out
}

// Sum is the hex SHA-256 of raw.
func Sum(raw []byte) string {
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}

// Parse reads raw. A message without a Message-ID gets <sha256@togavsu.invalid>.
func Parse(raw []byte) (msg *Message, err error) {
	if len(raw) > MaxBytes {
		return nil, ErrTooLarge
	}
	defer func() {
		if r := recover(); r != nil {
			msg, err = nil, fmt.Errorf("%w: %v", ErrMalformed, r)
		}
	}()
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && mr == nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	h := mr.Header
	m := &Message{RawSHA256: Sum(raw), RawSize: int64(len(raw))}
	if id, err := h.MessageID(); err == nil && id != "" {
		m.MessageID = "<" + id + ">"
	} else {
		m.MessageID = "<" + m.RawSHA256 + "@togavsu.invalid>"
	}
	if ids, err := h.MsgIDList("In-Reply-To"); err == nil && len(ids) > 0 {
		m.InReplyTo = "<" + ids[0] + ">"
	}
	if ids, err := h.MsgIDList("References"); err == nil {
		for _, id := range ids {
			m.References = append(m.References, "<"+id+">")
		}
	}
	m.FromHeader = decodeHeader(h.Get("From"))
	m.From = addrs(h, "From")
	m.To = addrs(h, "To")
	m.Cc = addrs(h, "Cc")
	for _, k := range []string{"Delivered-To", "Envelope-To", "X-Original-To"} {
		for _, v := range h.Values(k) {
			for _, a := range strings.Split(v, ",") {
				if a = strings.ToLower(strings.Trim(strings.TrimSpace(a), "<>")); a != "" {
					m.DeliveredTo = append(m.DeliveredTo, a)
				}
			}
		}
	}
	if s, err := h.Subject(); err == nil {
		m.Subject = s
	} else {
		m.Subject = h.Get("Subject")
	}
	if d, err := h.Date(); err == nil {
		m.Date = d
	}
	if mr != nil {
		if err := m.readParts(mr); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Message) readParts(mr *mail.Reader) error {
	idx := 0
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if m.Text != "" || m.HTML != "" || len(m.Parts) > 0 {
				// A broken trailing part keeps what was read before it.
				return nil
			}
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		body, err := io.ReadAll(io.LimitReader(p.Body, MaxBytes+1))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		switch ph := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := ph.ContentType()
			cid := strings.Trim(ph.Get("Content-Id"), "<> ")
			disp, dparams, _ := ph.ContentDisposition()
			switch {
			case cid != "" && !strings.HasPrefix(ct, "text/"):
				m.Parts = append(m.Parts, part(idx, dparams["filename"], ct, cid, true, body))
				idx++
			case ct == "text/plain" && m.Text == "" && disp != "attachment":
				m.Text = string(body)
			case ct == "text/html" && m.HTML == "" && disp != "attachment":
				m.HTML = string(body)
			case strings.HasPrefix(ct, "text/") && dparams["filename"] == "":
				// A further text alternative carries nothing new.
			default:
				m.Parts = append(m.Parts, part(idx, dparams["filename"], ct, cid, false, body))
				idx++
			}
		case *mail.AttachmentHeader:
			ct, _, _ := ph.ContentType()
			name, _ := ph.Filename()
			cid := strings.Trim(ph.Get("Content-Id"), "<> ")
			disp, _, _ := ph.ContentDisposition()
			inline := cid != "" && disp == "inline" && strings.HasPrefix(ct, "image/")
			m.Parts = append(m.Parts, part(idx, name, ct, cid, inline, body))
			idx++
		}
	}
}

func part(idx int, name, ct, cid string, inline bool, body []byte) Part {
	if name == "" {
		name = fmt.Sprintf("attachment-%d", idx+1)
		if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
			name += exts[0]
		}
	}
	return Part{Index: idx, Filename: name, MediaType: ct, Size: int64(len(body)), SHA256: Sum(body), Inline: inline, ContentID: cid, Data: body}
}

func addrs(h mail.Header, key string) []string {
	list, err := h.AddressList(key)
	if err != nil {
		// Fall back to anything that looks like an address.
		var out []string
		for _, f := range strings.FieldsFunc(h.Get(key), func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '<' || r == '>' }) {
			if strings.Contains(f, "@") {
				out = append(out, strings.ToLower(strings.Trim(f, `"'`)))
			}
		}
		return out
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, strings.ToLower(a.Address))
	}
	return out
}

var wordDecoder = mime.WordDecoder{}

func decodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// Snippet is the first 200 characters of text with whitespace folded.
func Snippet(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	r := []rune(s)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}
