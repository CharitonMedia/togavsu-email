// Package smtpsend sends one message over SMTP, bound to a context. It requires
// STARTTLS on every port but 465, where the connection is TLS from the start, so a
// password or token never crosses the network in the clear.
package smtpsend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
)

// ErrNoTLS is a server that offers no STARTTLS on a port that needs it.
var ErrNoTLS = errors.New("smtpsend: the server does not offer STARTTLS")

// ErrAuth is a server that refused the sign-in.
var ErrAuth = errors.New("smtpsend: the server refused the sign-in")

// Server is where and how to send.
type Server struct {
	Host string
	Port int
	// Login and Password sign in with AUTH PLAIN; Token signs in with XOAUTH2 instead.
	Login, Password, Token string
	// Implicit speaks TLS from the start (smtp_security tls); port 465 always does.
	Implicit bool
	// TLSConfig overrides the TLS settings; tests use it to trust a runtime test cert.
	TLSConfig *tls.Config
}

func (s Server) tlsConfig() *tls.Config {
	if s.TLSConfig != nil {
		c := s.TLSConfig.Clone()
		if c.ServerName == "" {
			c.ServerName = s.Host
		}
		return c
	}
	return &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
}

// Send delivers raw to every address in to, from from. The context bounds the whole
// exchange: cancelling it closes the connection.
func Send(ctx context.Context, s Server, from string, to []string, raw []byte) error {
	if len(to) == 0 {
		return errors.New("smtpsend: no recipients")
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	var d net.Dialer
	var conn net.Conn
	var err error
	implicit := s.Implicit || s.Port == 465
	if implicit {
		td := tls.Dialer{NetDialer: &d, Config: s.tlsConfig()}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return ctxErr(ctx, err)
	}
	defer c.Close()
	if !implicit {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return ErrNoTLS
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			return ctxErr(ctx, err)
		}
	}
	if s.Token != "" || s.Password != "" {
		var a smtp.Auth = plainAuth{s.Login, s.Password}
		if s.Token != "" {
			a = xoauth2{s.Login, s.Token}
		}
		if err := c.Auth(a); err != nil {
			var te *textproto.Error
			if errors.As(err, &te) && te.Code >= 500 && te.Code < 600 {
				return fmt.Errorf("%w: %v", ErrAuth, err)
			}
			return ctxErr(ctx, err)
		}
	}
	if err := c.Mail(from); err != nil {
		return ctxErr(ctx, err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return ctxErr(ctx, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return ctxErr(ctx, err)
	}
	if _, err := w.Write(raw); err != nil {
		return ctxErr(ctx, err)
	}
	if err := w.Close(); err != nil {
		return ctxErr(ctx, err)
	}
	return ctxErr(ctx, c.Quit())
}

func ctxErr(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// plainAuth is AUTH PLAIN over a connection that is already TLS (Send makes sure of
// that before it signs in).
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("smtpsend: unexpected server challenge")
	}
	return nil, nil
}

// xoauth2 is Google's and Microsoft's XOAUTH2 SASL mechanism.
type xoauth2 struct{ user, token string }

func (a xoauth2) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "XOAUTH2", []byte("user=" + a.user + "\x01auth=Bearer " + a.token + "\x01\x01"), nil
}

func (a xoauth2) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		// The server sent an error description; an empty answer ends the exchange.
		return []byte{}, nil
	}
	return nil, nil
}
