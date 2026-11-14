package render

import (
	"strings"
	"testing"
	"time"
)

// MOD-EML-22 and the sanitizer half of MOD-EML-32.
func TestSanitizeRemovesActiveContentAndKeepsLinks(t *testing.T) {
	src := `<html><head><style>body{x:expression(alert(1))}</style><script>alert(1)</script></head><body>
<p onclick="steal()" style="color:red">Hello <a href="https://court.example.test/notice/1">notice</a> and
<a href="mailto:clerk@court.example.test">clerk</a> and <a href="javascript:alert(1)">bad</a></p>
<img src="https://tracker.example.test/pixel.gif"><img src="cid:logo"><img src="data:image/png;base64,AAAA">
<div style="background:url(https://x.example.test/a)">bg</div><span style="width:expression(1)">e</span>
<svg><script>alert(2)</script></svg><iframe src="https://evil.example.test"></iframe><object data="x"></object>
<table border="1"><tr><td colspan="2">cell</td></tr></table><form><input value="x"></form>
<a href=" JaVaScRiPt:alert(3)">sneaky</a><math><mi>x</mi></math>
</body></html>`
	out := Sanitize(src)
	for _, bad := range []string{"<script", "onclick", "expression(", "tracker.example.test", "cid:logo", "url(", "<svg", "<iframe", "<object", "javascript:", "JaVaScRiPt", "<form", "<input", "<math", "<style"} {
		if strings.Contains(out, bad) {
			t.Errorf("sanitized output still holds %q:\n%s", bad, out)
		}
	}
	for _, good := range []string{`href="https://court.example.test/notice/1"`, `href="mailto:clerk@court.example.test"`, "<table", `colspan="2"`, "cell", `src="data:image/png;base64,AAAA"`, `style="color:red"`, ">bad</a>"} {
		if !strings.Contains(out, good) {
			t.Errorf("sanitized output lost %q:\n%s", good, out)
		}
	}
}

func TestDocumentEscapesHeaders(t *testing.T) {
	doc := Document(Header{From: `"<b>x</b>" <a@x.test>`, Subject: "<script>s</script>", Date: time.Unix(0, 0)}, "", "line <1>")
	if strings.Contains(doc, "<script>s") || strings.Contains(doc, "<b>x") || !strings.Contains(doc, "<pre class=\"email-body\">line &lt;1&gt;</pre>") {
		t.Fatal(doc)
	}
}

func TestFileName(t *testing.T) {
	d := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if got := FileName(d, "Re: Notice of Hearing — 26AB-CV00123!"); got != "email-2026-11-02-re-notice-of-hearing-26ab-cv00123.html" {
		t.Fatal(got)
	}
	if got := FileName(d, "   "); got != "email-2026-11-02-no-subject.html" {
		t.Fatal(got)
	}
}
