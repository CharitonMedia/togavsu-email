// Package email is the Togavsu Email module (togavsu-initial-modules-prd.md section 7,
// docs/SPEC.md): it connects the firm's existing mail (Gmail, Microsoft 365 and
// Outlook.com, any IMAP/SMTP server), reads it, sends it and files it to matters.
//
// The module runs in its own container. It reaches core records only through the core
// API with its scoped token and the context tokens the core hands each frame, and it
// reads and writes only its own schema (mod_email) through its own database login.
package email

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/filing"
	"github.com/CharitonMedia/togavsu-email/internal/mailbox"
	"github.com/CharitonMedia/togavsu-email/internal/packaging"
	"github.com/CharitonMedia/togavsu-email/internal/provider/all"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/server"
	"github.com/CharitonMedia/togavsu-email/internal/store"
	"github.com/CharitonMedia/togavsu-email/internal/syncer"
	pkgfiles "github.com/CharitonMedia/togavsu-email/package"
)

// ID is the module id from the manifest.
const ID = "email"

// Config is what the container gets: the core's address, its secrets and its own DB.
type Config struct {
	Core          string
	Token         string
	WebhookSecret string
	DB            *sql.DB
	HTTP          *http.Client
	Now           func() time.Time
	Log           *slog.Logger
	// Factory builds provider adapters; the default is the real Gmail, Graph and
	// IMAP/SMTP adapters. Tests pass the fake.
	Factory mailbox.Factory
	// OAuthToken and APIBase override provider endpoints (tests).
	OAuthToken string
	APIBase    map[string]string
	// TempDir holds raw working copies while a filing runs.
	TempDir string
}

// Module is the wired module.
type Module struct {
	Store     *store.Store
	Core      *coreapi.Client
	Opener    *mailbox.Opener
	Filer     *filing.Filer
	Sender    *send.Sender
	Processor *syncer.Processor
	Syncer    *syncer.Syncer
	Server    *server.Server
}

// New wires the module.
func New(c Config) *Module {
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Factory == nil {
		c.Factory = all.Factory(c.HTTP)
	}
	st := store.New(c.DB, c.Now)
	core := &coreapi.Client{Base: c.Core, Token: c.Token, HTTP: c.HTTP}
	settings := func(ctx context.Context) (config.Settings, error) {
		m, err := core.Settings(ctx)
		if err != nil {
			return config.Settings{}, err
		}
		return config.Parse(m)
	}
	op := &mailbox.Opener{Core: core, Factory: c.Factory, Settings: settings, TokenURL: c.OAuthToken, APIBase: c.APIBase}
	filer := &filing.Filer{Store: st, Core: core, Mailbox: op.Open, TempDir: c.TempDir}
	sender := &send.Sender{Store: st, Core: core, Open: op.Open, Filer: filer, Settings: settings, Now: c.Now}
	m := &Module{Store: st, Core: core, Opener: op, Filer: filer, Sender: sender}
	srv := &server.Server{Store: st, Core: core, Filer: filer, Sender: sender, Open: op.Open, Settings: settings, WebhookSecret: c.WebhookSecret,
		Now: c.Now, Log: c.Log, HTTP: c.HTTP, OAuthToken: c.OAuthToken, APIBase: c.APIBase}
	m.Processor = &syncer.Processor{Store: st, Core: core, Filer: filer, AIDenied: srv.AIDenied}
	m.Syncer = &syncer.Syncer{Store: st, Core: core, Open: op.Open, Processor: m.Processor, Sender: sender, Settings: settings, Now: c.Now, Log: c.Log}
	srv.Syncer = m.Syncer
	srv.CheckAccount = func(ctx context.Context, a store.Account) error {
		p, err := op.Open(ctx, a)
		if err != nil {
			return err
		}
		return p.Check(ctx)
	}
	m.Server = srv
	return m
}

// Handler serves every module path.
func (m *Module) Handler() http.Handler { return m.Server.Handler() }

// Run runs the sync loop until ctx ends.
func (m *Module) Run(ctx context.Context) { m.Syncer.Run(ctx) }

// Files returns the package files (manifest and migrations) by package path.
func Files() (map[string][]byte, error) { return pkgfiles.Files() }

// Archive packs the package files as the gzip tar the core installs, through the core's
// public module kit. edit, when set, changes the files first (tests use it).
func Archive(edit func(map[string][]byte)) ([]byte, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edit(files)
	}
	return packaging.Archive(files)
}

// Build writes the package signed with key as the signed package file the admin
// installs.
func Build(key ed25519.PrivateKey, edit func(map[string][]byte)) ([]byte, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edit(files)
	}
	return packaging.Sign(key, files)
}
