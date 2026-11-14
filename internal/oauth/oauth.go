// Package oauth runs the OAuth 2.0 authorization code flow with PKCE for Gmail and
// Microsoft accounts, and refreshes their tokens. Endpoints can be overridden so tests
// point them at httptest servers on 127.0.0.1.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/provider"
)

// Scopes per provider.
const (
	GmailScope     = "https://www.googleapis.com/auth/gmail.modify"
	MicrosoftScope = "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send offline_access"
)

// Endpoints are one provider's authorize and token URLs.
type Endpoints struct {
	AuthURL  string
	TokenURL string
}

// For returns the provider's endpoints; tokenOverride replaces the token URL (tests).
func For(prov, tenant string) Endpoints {
	switch prov {
	case "gmail":
		return Endpoints{AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token"}
	case "microsoft":
		if tenant == "" {
			tenant = "organizations"
		}
		base := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/"
		return Endpoints{AuthURL: base + "authorize", TokenURL: base + "token"}
	}
	return Endpoints{}
}

// Scope is the provider's scope string.
func Scope(prov string) string {
	if prov == "gmail" {
		return GmailScope
	}
	return MicrosoftScope
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// NewVerifier returns a PKCE code verifier and a state value.
func NewVerifier() (verifier, state string) { return random(verifierBytes), random(stateBytes) }

// Challenge is the S256 PKCE challenge for verifier.
func Challenge(verifier string) string {
	s := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// AuthURL is the consent page the user opens in a top-level window.
func AuthURL(e Endpoints, prov, clientID, redirect, state, verifier, loginHint string) string {
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "scope": {Scope(prov)},
		"state": {state}, "code_challenge": {Challenge(verifier)}, "code_challenge_method": {"S256"}}
	if prov == "gmail" {
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	} else {
		q.Set("response_mode", "query")
	}
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	return e.AuthURL + "?" + q.Encode()
}

// ErrRefused is a token endpoint refusal (a bad code, a revoked refresh token).
var ErrRefused = errors.New("oauth: the token endpoint refused the request")

func tokenCall(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (provider.Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return provider.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return provider.Token{}, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= http.StatusInternalServerError {
		return provider.Token{}, fmt.Errorf("%w: token endpoint %s", provider.ErrUnavailable, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.Token{}, fmt.Errorf("%w (%s)", ErrRefused, resp.Status)
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return provider.Token{}, fmt.Errorf("%w: no access token in the answer", ErrRefused)
	}
	return provider.Token{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: time.Now().Add(time.Duration(t.ExpiresIn) * second)}, nil
}

// Exchange trades an authorization code for tokens.
func Exchange(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, code, redirect, verifier string) (provider.Token, error) {
	return tokenCall(ctx, hc, tokenURL, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "client_secret": {clientSecret}, "code_verifier": {verifier}})
}

// Refresh gets a new access token; a refresh token the answer omits is kept.
func Refresh(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret string, t provider.Token) (provider.Token, error) {
	if t.RefreshToken == "" {
		return provider.Token{}, provider.ErrAuth
	}
	nt, err := tokenCall(ctx, hc, tokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken},
		"client_id": {clientID}, "client_secret": {clientSecret}})
	if errors.Is(err, ErrRefused) {
		return provider.Token{}, fmt.Errorf("%w: %v", provider.ErrAuth, err)
	}
	if err != nil {
		return provider.Token{}, err
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = t.RefreshToken
	}
	return nt, nil
}

// Source hands out a valid access token, refreshing and saving it when it expires.
type Source struct {
	HTTP                   *http.Client
	TokenURL               string
	ClientID, ClientSecret string
	Token                  provider.Token
	Save                   func(ctx context.Context, t provider.Token) error
}

// Access returns the access token, refreshing it when it is within a minute of expiry
// or force is set (the API answered 401).
func (s *Source) Access(ctx context.Context, force bool) (string, error) {
	if !force && s.Token.AccessToken != "" && (s.Token.Expiry.IsZero() || time.Until(s.Token.Expiry) > refreshMargin) {
		return s.Token.AccessToken, nil
	}
	nt, err := Refresh(ctx, s.HTTP, s.TokenURL, s.ClientID, s.ClientSecret, s.Token)
	if err != nil {
		return "", err
	}
	s.Token = nt
	if s.Save != nil {
		if err := s.Save(ctx, nt); err != nil {
			return "", err
		}
	}
	return nt.AccessToken, nil
}

// API bases.
const (
	GmailAPI = "https://gmail.googleapis.com"
	GraphAPI = "https://graph.microsoft.com"
)

// Profile asks the provider which mailbox an access token is for: Gmail's profile
// emailAddress, or Graph's mail (userPrincipalName when mail is empty).
func Profile(ctx context.Context, hc *http.Client, prov, apiBase, access string) (string, error) {
	u := GmailAPI + "/gmail/v1/users/me/profile"
	if prov == "microsoft" {
		u = GraphAPI + "/v1.0/me?$select=mail,userPrincipalName"
	}
	if apiBase != "" {
		u = strings.TrimRight(apiBase, "/") + strings.TrimPrefix(strings.TrimPrefix(u, GmailAPI), GraphAPI)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth: profile: %s", resp.Status)
	}
	var p struct {
		EmailAddress      string `json:"emailAddress"`
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return "", err
	}
	for _, a := range []string{p.EmailAddress, p.Mail, p.UserPrincipalName} {
		if strings.Contains(a, "@") {
			return a, nil
		}
	}
	return "", errors.New("oauth: the profile has no address")
}
