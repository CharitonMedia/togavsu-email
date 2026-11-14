// Package render cleans message HTML with an allowlist and writes the filed HTML copy:
// a header block over the sanitized body, every remote resource stripped, links live.
package render

import (
	"bytes"
	"html"
	"regexp"
	"strings"
	"time"

	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// PreviewCSP is the Content-Security-Policy of the queue preview.
const PreviewCSP = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox"

// dropWhole removes the element and everything inside it.
var dropWhole = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Iframe: true, atom.Object: true, atom.Embed: true,
	atom.Svg: true, atom.Math: true, atom.Noscript: true, atom.Template: true, atom.Head: true,
	atom.Title: true, atom.Form: true, atom.Input: true, atom.Button: true, atom.Select: true,
	atom.Textarea: true, atom.Link: true, atom.Meta: true, atom.Base: true, atom.Frame: true,
	atom.Frameset: true, atom.Applet: true, atom.Audio: true, atom.Video: true, atom.Source: true,
	atom.Picture: true, atom.Canvas: true, atom.Dialog: true,
}

// keep lists the elements that survive; any other element is replaced by its children.
var keep = map[atom.Atom]bool{
	atom.A: true, atom.Abbr: true, atom.B: true, atom.Blockquote: true, atom.Br: true, atom.Caption: true,
	atom.Center: true, atom.Code: true, atom.Col: true, atom.Colgroup: true, atom.Dd: true, atom.Del: true,
	atom.Div: true, atom.Dl: true, atom.Dt: true, atom.Em: true, atom.Font: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Hr: true, atom.I: true, atom.Img: true,
	atom.Ins: true, atom.Li: true, atom.Ol: true, atom.P: true, atom.Pre: true, atom.S: true, atom.Small: true,
	atom.Span: true, atom.Strike: true, atom.Strong: true, atom.Sub: true, atom.Sup: true, atom.Table: true,
	atom.Tbody: true, atom.Td: true, atom.Tfoot: true, atom.Th: true, atom.Thead: true, atom.Tr: true,
	atom.U: true, atom.Ul: true, atom.Address: true, atom.Cite: true, atom.Q: true, atom.Tt: true,
}

var attrs = map[string]bool{
	"href": true, "src": true, "alt": true, "title": true, "width": true, "height": true, "align": true,
	"valign": true, "colspan": true, "rowspan": true, "border": true, "cellpadding": true, "cellspacing": true,
	"bgcolor": true, "color": true, "face": true, "size": true, "style": true, "dir": true, "lang": true,
}

var badStyle = regexp.MustCompile(`(?i)expression\s*\(|url\s*\(|javascript:|behavior\s*:|-moz-binding|@import`)

// safeHref reports whether a link target may stay live.
func safeHref(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.Map(func(r rune) rune {
		if r < ' ' {
			return -1
		}
		return r
	}, s)
	for _, p := range []string{"http://", "https://", "mailto:", "tel:", "#"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return !strings.Contains(s, ":") // a relative link
}

// Sanitize returns body HTML with only allowlisted elements and attributes, no script,
// no event handler, no remote resource and every safe link kept.
func Sanitize(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	ctx := &nethtml.Node{Type: nethtml.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := nethtml.ParseFragment(strings.NewReader(src), ctx)
	if err != nil {
		return html.EscapeString(src)
	}
	var buf bytes.Buffer
	for _, n := range nodes {
		write(&buf, n)
	}
	return buf.String()
}

func write(buf *bytes.Buffer, n *nethtml.Node) {
	switch n.Type {
	case nethtml.TextNode:
		buf.WriteString(html.EscapeString(n.Data))
		return
	case nethtml.ElementNode:
	case nethtml.DocumentNode:
		children(buf, n)
		return
	default:
		return // comments, doctypes
	}
	if dropWhole[n.DataAtom] || n.DataAtom == zero && (strings.Contains(n.Data, ":") || n.Data == "svg") {
		return
	}
	if !keep[n.DataAtom] {
		children(buf, n)
		return
	}
	if n.DataAtom == atom.Img {
		src := ""
		for _, a := range n.Attr {
			if strings.ToLower(a.Key) == "src" {
				src = strings.TrimSpace(a.Val)
			}
		}
		if !strings.HasPrefix(strings.ToLower(src), "data:image/") {
			return // remote and cid images are not loaded
		}
	}
	buf.WriteString("<" + n.Data)
	for _, a := range n.Attr {
		k := strings.ToLower(a.Key)
		if a.Namespace != "" || !attrs[k] {
			continue
		}
		v := a.Val
		switch k {
		case "href":
			if n.DataAtom != atom.A || !safeHref(v) {
				continue
			}
		case "src":
			if n.DataAtom != atom.Img {
				continue
			}
		case "style":
			if badStyle.MatchString(v) {
				continue
			}
		}
		buf.WriteString(" " + k + `="` + html.EscapeString(v) + `"`)
	}
	if n.DataAtom == atom.A {
		buf.WriteString(` rel="noopener noreferrer" target="_blank"`)
	}
	buf.WriteString(">")
	if voidElement(n.DataAtom) {
		return
	}
	children(buf, n)
	buf.WriteString("</" + n.Data + ">")
}

func children(buf *bytes.Buffer, n *nethtml.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		write(buf, c)
	}
}

func voidElement(a atom.Atom) bool {
	return a == atom.Br || a == atom.Hr || a == atom.Img || a == atom.Col
}

// Header is the header block of a filed or previewed message.
type Header struct {
	From, To, Cc, Subject string
	Date                  time.Time
}

// Document writes the self-contained HTML copy: the header block over the sanitized
// HTML body, or the text body in <pre> when there is no HTML.
func Document(h Header, sanitizedHTML, text string) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>")
	b.WriteString(html.EscapeString(h.Subject))
	b.WriteString("</title></head><body>\n<table class=\"email-header\">\n")
	row := func(k, v string) {
		if v == "" {
			return
		}
		b.WriteString("<tr><th align=\"left\">" + k + "</th><td>" + html.EscapeString(v) + "</td></tr>\n")
	}
	row("From", h.From)
	row("To", h.To)
	row("Cc", h.Cc)
	if !h.Date.IsZero() {
		row("Date", h.Date.Format(time.RFC1123Z))
	}
	row("Subject", h.Subject)
	b.WriteString("</table>\n<hr>\n")
	if strings.TrimSpace(sanitizedHTML) != "" {
		b.WriteString("<div class=\"email-body\">" + sanitizedHTML + "</div>\n")
	} else {
		b.WriteString("<pre class=\"email-body\">" + html.EscapeString(text) + "</pre>\n")
	}
	b.WriteString("</body></html>\n")
	return b.String()
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// FileName is the filed copy's name: email-<YYYY-MM-DD>-<subject words>.html.
func FileName(date time.Time, subject string) string {
	words := strings.Fields(nonWord.ReplaceAllString(strings.ToLower(subject), " "))
	if len(words) > fileNameWords {
		words = words[:fileNameWords]
	}
	slug := strings.Join(words, "-")
	if len(slug) > fileNameSlugMax {
		slug = strings.Trim(slug[:fileNameSlugMax], "-")
	}
	if slug == "" {
		slug = "no-subject"
	}
	if date.IsZero() {
		date = time.Now()
	}
	return "email-" + date.UTC().Format("2006-01-02") + "-" + slug + ".html"
}

// Text reads the visible text of an HTML body, for a note when the message has no text
// part.
func Text(src string) string {
	doc, err := nethtml.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n.Type == nethtml.ElementNode && (n.DataAtom == atom.Script || n.DataAtom == atom.Style || n.DataAtom == atom.Head) {
			return
		}
		if n.Type == nethtml.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == nethtml.ElementNode {
			switch n.DataAtom {
			case atom.P, atom.Div, atom.Br, atom.Tr, atom.Li, atom.H1, atom.H2, atom.H3, atom.H4, atom.Table:
				b.WriteString("\n")
			}
		}
	}
	walk(doc)
	lines := strings.Split(b.String(), "\n")
	var out []string
	blank := false
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
