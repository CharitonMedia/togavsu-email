// Package restapi is the HTTP plumbing the Gmail and Graph adapters share: a bearer
// token from oauth.Source, one refresh and retry on 401, waiting out a 429 or 503 for
// as long as the server's Retry-After asks, and mapping failures to the provider
// errors.
package restapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/oauth"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
)

// MaxThrottled is how many throttled answers in a row one call waits out before it
// gives up with provider.ErrUnavailable.
const MaxThrottled = 3

// StatusError is an answer the caller may want to tell apart (404, 410).
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("restapi: HTTP %d: %s", e.Status, e.Body) }

// Is lets a 404 match provider.ErrNotFound.
func (e *StatusError) Is(target error) bool {
	return target == provider.ErrNotFound && e.Status == http.StatusNotFound
}

// StatusOf returns the HTTP status of err, or 0.
func StatusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// Client calls one provider API as one account.
type Client struct {
	HTTP   *http.Client
	Source *oauth.Source
	// Sleep waits out a Retry-After; tests replace it to record the wait.
	Sleep func(ctx context.Context, d time.Duration) error
	// Extra headers on every call (Graph's Prefer: IdType).
	Header http.Header
}

// New builds a client for the config's token, refreshing at tokenURL.
func New(hc *http.Client, c provider.Config, tokenURL string) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	src := &oauth.Source{HTTP: hc, TokenURL: tokenURL, ClientID: c.ClientID, ClientSecret: c.ClientSecret, Save: c.SaveToken}
	if c.Token != nil {
		src.Token = *c.Token
	}
	return &Client{HTTP: hc, Source: src, Sleep: sleep}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryAfter reads a Retry-After header: seconds or an HTTP date.
func RetryAfter(h string, now time.Time) (time.Duration, bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(h); err == nil && n >= 0 {
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// Request is one call.
type Request struct {
	Method, URL string
	// ContentType and Body; a nil Body with JSON set sends JSON.
	ContentType string
	Body        []byte
	JSON        any
	// NoAuth leaves the bearer token off (Graph's pre-authorized upload URLs).
	NoAuth bool
	Header http.Header
}

// Do runs r and returns the answer's body. Out, when not nil, gets the JSON body.
func (c *Client) Do(ctx context.Context, r Request, out any) ([]byte, error) {
	body, ctype := r.Body, r.ContentType
	if r.JSON != nil {
		b, err := json.Marshal(r.JSON)
		if err != nil {
			return nil, err
		}
		body, ctype = b, "application/json"
	}
	refreshed, throttled := false, 0
	for {
		var tok string
		if !r.NoAuth {
			var err error
			tok, err = c.Source.Access(ctx, false)
			if err != nil {
				return nil, c.authErr(ctx, err)
			}
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if body == nil {
			req.Body = nil
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		for _, hs := range []http.Header{c.Header, r.Header} {
			for k, v := range hs {
				req.Header[k] = v
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !r.NoAuth:
			if refreshed {
				return nil, provider.ErrAuth
			}
			refreshed = true
			if _, err := c.Source.Access(ctx, true); err != nil {
				return nil, c.authErr(ctx, err)
			}
			continue
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			d, ok := RetryAfter(resp.Header.Get("Retry-After"), time.Now())
			throttled++
			if !ok || throttled > MaxThrottled {
				return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUnavailable, resp.StatusCode)
			}
			if err := c.Sleep(ctx, d); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode >= 500:
			return nil, fmt.Errorf("%w: HTTP %d", provider.ErrUnavailable, resp.StatusCode)
		case resp.StatusCode >= 300:
			return nil, &StatusError{Status: resp.StatusCode, Body: trim(data)}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return nil, fmt.Errorf("restapi: bad answer from %s: %w", r.URL, err)
			}
		}
		return data, nil
	}
}

func (c *Client) authErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, oauth.ErrRefused) || errors.Is(err, provider.ErrAuth) {
		return provider.ErrAuth
	}
	return fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
}

func trim(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// HeaderAddresses reads the To and Cc addresses of raw, lower-cased.
func HeaderAddresses(raw []byte) map[string]bool {
	out := map[string]bool{}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return out
	}
	for _, k := range []string{"To", "Cc"} {
		if l, err := m.Header.AddressList(k); err == nil {
			for _, a := range l {
				out[strings.ToLower(a.Address)] = true
			}
		}
	}
	return out
}

// WithBcc puts a Bcc header on raw for every envelope recipient its To and Cc don't
// name. Gmail and Graph take recipients from the headers and strip Bcc before
// delivery, so this is how a blind copy reaches them.
func WithBcc(raw []byte, env provider.Envelope) []byte {
	named := HeaderAddresses(raw)
	var bcc []string
	for _, a := range env.To {
		if !named[strings.ToLower(a)] {
			bcc = append(bcc, a)
		}
	}
	if len(bcc) == 0 {
		return raw
	}
	return append([]byte("Bcc: "+strings.Join(bcc, ", ")+"\r\n"), raw...)
}

// MessageID is raw's Message-ID header.
func MessageID(raw []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(m.Header.Get("Message-Id"))
}
