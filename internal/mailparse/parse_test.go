package mailparse

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu-email/internal/testmail"
)

func TestMixedWithAttachmentsAndInline(t *testing.T) {
	raw := testmail.Msg{ID: "a1@client.example.test", From: "Pat Doe <Pat@Client.Example.Test>", To: []string{"filing@firm.example.test"},
		Subject: "Lease", Text: "See attached.", HTML: `<p>See <img src="cid:logo1"></p>`,
		Attachments: []testmail.Attachment{{Name: "lease.pdf", Type: "application/pdf", Data: testmail.PDF(1)},
			{Name: "rider.pdf", Type: "application/pdf", Data: testmail.PDF(2)},
			{Name: "logo.png", Type: "image/png", Data: testmail.PNG, ContentID: "logo1"}}}.Raw()
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.MessageID != "<a1@client.example.test>" || m.From[0] != "pat@client.example.test" || m.Text != "See attached." || !strings.Contains(m.HTML, "cid:logo1") {
		t.Fatalf("headers or body: %+v", m)
	}
	if len(m.Parts) != 3 || len(m.Attachments()) != 2 || !m.Parts[2].Inline || m.Parts[2].ContentID != "logo1" {
		t.Fatalf("parts: %+v", m.Parts)
	}
	if !bytes.Equal(m.Parts[0].Data, testmail.PDF(1)) || m.Parts[0].SHA256 != Sum(testmail.PDF(1)) {
		t.Fatal("attachment bytes")
	}
}

func TestEncodingsAndCharsets(t *testing.T) {
	raw := "From: =?UTF-8?B?UMOpdA==?= <pet@client.example.test>\r\nTo: a@firm.example.test\r\nSubject: =?ISO-8859-1?Q?Caf=E9_meeting?=\r\n" +
		"Message-ID: <q1@x.test>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nCaf=E9 at noon=\r\n today\r\n"
	m, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Café meeting" || !strings.Contains(m.Text, "Café at noon today") || !strings.Contains(m.FromHeader, "Pét") {
		t.Fatalf("decoded: %q %q %q", m.Subject, m.Text, m.FromHeader)
	}
	b64 := "From: a@client.example.test\r\nSubject: b\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybGQ=\r\n"
	m, err = Parse([]byte(b64))
	if err != nil || m.Text != "hello world" {
		t.Fatalf("base64 body: %v %q", err, m.Text)
	}
}

func TestMissingMessageIDIsDerived(t *testing.T) {
	raw := testmail.Msg{From: "a@client.example.test", Subject: "x", Text: "y"}.Raw()
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.MessageID != "<"+Sum(raw)+"@togavsu.invalid>" {
		t.Fatalf("derived id %s", m.MessageID)
	}
}

func TestSizeLimits(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 30<<20)
	raw := testmail.Msg{ID: "big@x.test", From: "a@client.example.test", Subject: "big",
		Attachments: []testmail.Attachment{{Name: "big.bin", Data: big}}}.Raw()
	m, err := Parse(raw)
	if err != nil || len(m.Attachments()) != 1 || m.Attachments()[0].Size != int64(len(big)) {
		t.Fatalf("30 MB message: %v", err)
	}
	if _, err := Parse(make([]byte, MaxBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over 50 MB: %v", err)
	}
}

func TestMalformedDoesNotPanic(t *testing.T) {
	for _, raw := range []string{
		"Content-Type: multipart/mixed; boundary=\"x\"\r\n\r\n--x\r\nContent-Type: text/plain\r\n",
		"\x00\x01\x02",
		"Content-Type: multipart/mixed\r\n\r\nno boundary",
		"From: <<<\r\nTo: ,,,\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!!",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", raw, r)
				}
			}()
			_, _ = Parse([]byte(raw))
		}()
	}
}
