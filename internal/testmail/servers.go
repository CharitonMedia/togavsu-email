package testmail

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// Loopback is the only host a test may point an adapter at.
const Loopback = "127.0.0.1"

// MustLoopback panics on any host but 127.0.0.1, so a test can never reach a real
// mail server by a configuration mistake.
func MustLoopback(host string) {
	if host != Loopback {
		panic("testmail: tests may only use 127.0.0.1, not " + host)
	}
}

// Cert makes a throwaway self-signed certificate for 127.0.0.1 at runtime, and a
// client config that trusts it. Nothing is written to disk.
func Cert() (server, client *tls.Config) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(one), Subject: pkix.Name{CommonName: Loopback},
		NotBefore: time.Now().Add(-hour), NotAfter: time.Now().Add(certLife),
		IPAddresses: []net.IP{net.ParseIP(Loopback)}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}
	client = &tls.Config{RootCAs: pool, ServerName: Loopback}
	return server, client
}

// SMTPMessage is one message an SMTPServer accepted.
type SMTPMessage struct {
	From string
	To   []string
	Data []byte
	// Auth is the SASL mechanism and the user it named.
	Mech, User string
	TLS        bool
}

// SMTPServer is a small SMTP listener on 127.0.0.1 for tests. It offers STARTTLS
// when StartTLS is set, or speaks TLS from the start when Implicit is set, takes AUTH
// PLAIN and XOAUTH2 for User with Password or Token, and records each message.
type SMTPServer struct {
	Addr                  string
	Port                  int
	User, Password, Token string

	tls      *tls.Config
	implicit bool
	starttls bool
	ln       net.Listener
	mu       sync.Mutex
	msgs     []SMTPMessage
}

// NewSMTP starts a listener. With implicit true it is TLS from the start; otherwise
// it offers STARTTLS when starttls is true and plain text only when false.
func NewSMTP(cfg *tls.Config, implicit, starttls bool) *SMTPServer {
	ln, err := net.Listen("tcp", Loopback+":0")
	if err != nil {
		panic(err)
	}
	if implicit {
		ln = tls.NewListener(ln, cfg)
	}
	s := &SMTPServer{ln: ln, tls: cfg, implicit: implicit, starttls: starttls, Addr: ln.Addr().String(),
		Port: ln.Addr().(*net.TCPAddr).Port}
	go s.serve()
	return s
}

// Messages returns what the server has accepted so far.
func (s *SMTPServer) Messages() []SMTPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SMTPMessage(nil), s.msgs...)
}

// Close stops the listener.
func (s *SMTPServer) Close() { s.ln.Close() }

func (s *SMTPServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(c)
	}
}

func (s *SMTPServer) session(c net.Conn) {
	defer c.Close()
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(f string, a ...any) { fmt.Fprintf(w, f+"\r\n", a...); w.Flush() }
	isTLS := s.implicit
	var m SMTPMessage
	say("220 localhost test SMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", two)[zero])
		arg := strings.TrimSpace(strings.TrimPrefix(line, strings.SplitN(line, " ", two)[zero]))
		switch verb {
		case "EHLO", "HELO":
			fmt.Fprintf(w, "250-localhost\r\n")
			if s.starttls && !isTLS {
				fmt.Fprintf(w, "250-STARTTLS\r\n")
			}
			say("250 AUTH PLAIN XOAUTH2")
		case "STARTTLS":
			if !s.starttls || isTLS {
				say("502 not offered")
				continue
			}
			say("220 go ahead")
			tc := tls.Server(c, s.tls)
			if tc.Handshake() != nil {
				return
			}
			c, isTLS = tc, true
			r, w = bufio.NewReader(c), bufio.NewWriter(c)
		case "AUTH":
			mech, ir, _ := strings.Cut(arg, " ")
			b, _ := base64.StdEncoding.DecodeString(ir)
			ok := false
			switch strings.ToUpper(mech) {
			case "PLAIN":
				p := strings.Split(string(b), "\x00")
				ok = len(p) == authPlainParts && p[one] == s.User && s.Password != "" && p[two] == s.Password
				if len(p) == authPlainParts {
					m.User = p[one]
				}
			case "XOAUTH2":
				f := strings.Split(string(b), "\x01")
				ok = len(f) >= two && f[zero] == "user="+s.User && s.Token != "" && f[one] == "auth=Bearer "+s.Token
				m.User = strings.TrimPrefix(f[0], "user=")
			}
			m.Mech = strings.ToUpper(mech)
			if ok {
				say("235 ok")
			} else {
				say("535 authentication failed")
			}
		case "MAIL":
			m.From = strings.Trim(strings.TrimPrefix(strings.ToUpper(arg[:len(mailFrom)])+arg[len(mailFrom):], mailFrom), "<> ")
			say("250 ok")
		case "RCPT":
			m.To = append(m.To, strings.Trim(arg[len(rcptTo):], "<> "))
			say("250 ok")
		case "DATA":
			say("354 send it")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
			}
			m.Data, m.TLS = []byte(data.String()), isTLS
			s.mu.Lock()
			s.msgs = append(s.msgs, m)
			s.mu.Unlock()
			m = SMTPMessage{Mech: m.Mech, User: m.User}
			say("250 queued")
		case "RSET", "NOOP":
			say("250 ok")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("500 unknown")
		}
	}
}
