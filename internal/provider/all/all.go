// Package all picks the adapter for an account's provider.
package all

import (
	"context"
	"errors"
	"net/http"

	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/provider/gmail"
	"github.com/CharitonMedia/togavsu-email/internal/provider/graph"
	"github.com/CharitonMedia/togavsu-email/internal/provider/imapsmtp"
)

// Factory returns the real adapters' factory.
func Factory(hc *http.Client) func(ctx context.Context, c provider.Config) (provider.Provider, error) {
	return func(_ context.Context, c provider.Config) (provider.Provider, error) {
		switch c.Provider {
		case "gmail":
			return gmail.New(c, hc), nil
		case "microsoft":
			return graph.New(c, hc), nil
		case "imap":
			return imapsmtp.New(c), nil
		}
		return nil, errors.New("provider: unknown provider " + c.Provider)
	}
}
