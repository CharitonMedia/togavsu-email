// Package mailbox opens the provider adapter for a stored account, with its
// credentials read from the core's module secret store (CC-2). Credentials live only in
// memory for the call.
package mailbox

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// Factory builds an adapter from a config.
type Factory func(ctx context.Context, c provider.Config) (provider.Provider, error)

// Secret names under an account's secret_ref.
const (
	PasswordSecret = "password"
	TokenSecret    = "token"
)

// SecretName is the core secret name for one of an account's secrets.
func SecretName(a store.Account, kind string) string { return a.SecretRef + "/" + kind }

// Opener opens accounts.
type Opener struct {
	Core     *coreapi.Client
	Factory  Factory
	Settings func(ctx context.Context) (config.Settings, error)
	// TokenURL and APIBase override the providers' endpoints (tests).
	TokenURL, APIBase string
}

// Open returns the adapter for a.
func (o *Opener) Open(ctx context.Context, a store.Account) (provider.Provider, error) {
	c := provider.Config{AccountID: a.ID, Provider: a.Provider, Address: a.Address, IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort,
		IMAPSecurity: a.IMAPSecurity, SMTPHost: a.SMTPHost, SMTPPort: a.SMTPPort, SMTPSecurity: a.SMTPSecurity, Login: a.Login,
		TokenURL: o.TokenURL, APIBase: o.APIBase}
	if a.SecretRef == "" {
		return nil, errors.New("mailbox: the account has no stored credentials")
	}
	if a.Provider == "imap" {
		pw, err := o.Core.GetSecret(ctx, SecretName(a, PasswordSecret))
		if err != nil && !coreapi.IsNotFound(err) {
			return nil, err
		}
		c.Password = pw
	}
	if a.Provider != "imap" || c.Password == "" {
		raw, err := o.Core.GetSecret(ctx, SecretName(a, TokenSecret))
		if coreapi.IsNotFound(err) {
			return nil, provider.ErrAuth
		}
		if err != nil {
			return nil, err
		}
		var t provider.Token
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, provider.ErrAuth
		}
		c.Token = &t
		c.XOAUTH2 = a.Provider == "imap"
		c.SaveToken = func(ctx context.Context, t provider.Token) error {
			b, _ := json.Marshal(t)
			return o.Core.PutSecret(ctx, SecretName(a, TokenSecret), string(b))
		}
	}
	if o.Settings != nil {
		s, err := o.Settings(ctx)
		if err != nil {
			return nil, err
		}
		switch a.Provider {
		case "gmail":
			c.ClientID, c.ClientSecret = s.GoogleClientID, s.GoogleClientSecret
		case "microsoft":
			c.ClientID, c.ClientSecret, c.Tenant = s.MicrosoftClientID, s.MicrosoftClientSecret, s.MicrosoftTenant
		}
	}
	return o.Factory(ctx, c)
}
