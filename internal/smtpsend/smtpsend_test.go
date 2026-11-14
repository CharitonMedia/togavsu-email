package smtpsend

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu-email/internal/testmail"
)

func fakeSecret() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func server(t *testing.T, implicit, starttls bool) (*testmail.SMTPServer, Server) {
	t.Helper()
	scfg, ccfg := testmail.Cert()
	ts := testmail.NewSMTP(scfg, implicit, starttls)
	t.Cleanup(ts.Close)
	ts.User, ts.Password = "clerk@firm.test", fakeSecret()
	testmail.MustLoopback(testmail.Loopback)
	return ts, Server{Host: testmail.Loopback, Port: ts.Port, Login: ts.User, Password: ts.Password, TLSConfig: ccfg}
}

const raw = "From: clerk@firm.test\r\nTo: a@client.test\r\nSubject: hi\r\n\r\n.leading dot\r\nbody\r\n"

func TestSendWithStartTLS(t *testing.T) {
	ts, s := server(t, false, true)
	if err := Send(context.Background(), s, "clerk@firm.test", []string{"a@client.test", "b@client.test"}, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	m := ts.Messages()
	if len(m) != 1 || !m[0].TLS || m[0].Mech != "PLAIN" || len(m[0].To) != 2 || m[0].From != "clerk@firm.test" {
		t.Fatalf("got %+v", m)
	}
	if !strings.Contains(string(m[0].Data), "\r\n.leading dot\r\n") {
		t.Fatalf("dot stuffing lost: %q", m[0].Data)
	}
}

func TestStartTLSRequired(t *testing.T) {
	ts, s := server(t, false, false)
	err := Send(context.Background(), s, "clerk@firm.test", []string{"a@client.test"}, []byte(raw))
	if !errors.Is(err, ErrNoTLS) {
		t.Fatalf("want ErrNoTLS, got %v", err)
	}
	if len(ts.Messages()) != 0 {
		t.Fatal("sent without TLS")
	}
}

func TestImplicitTLS(t *testing.T) {
	ts, s := server(t, true, false)
	s.Implicit = true
	if err := Send(context.Background(), s, "clerk@firm.test", []string{"a@client.test"}, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if m := ts.Messages(); len(m) != 1 || !m[0].TLS {
		t.Fatalf("got %+v", m)
	}
}

func TestXOAUTH2AndRefusedSignIn(t *testing.T) {
	ts, s := server(t, false, true)
	ts.Token = fakeSecret()
	s.Password, s.Token = "", ts.Token
	if err := Send(context.Background(), s, "clerk@firm.test", []string{"a@client.test"}, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if m := ts.Messages(); len(m) != 1 || m[0].Mech != "XOAUTH2" {
		t.Fatalf("got %+v", m)
	}
	s.Token = "wrong"
	if err := Send(context.Background(), s, "clerk@firm.test", []string{"a@client.test"}, []byte(raw)); !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

func TestCancelledContext(t *testing.T) {
	_, s := server(t, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Send(ctx, s, "clerk@firm.test", []string{"a@client.test"}, []byte(raw)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
