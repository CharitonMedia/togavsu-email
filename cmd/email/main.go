// Command email runs the Email module, or packages it.
//
//	email serve -core http://core:8080 -secrets /run/secrets -db-host postgres:5432 -db-name togavsu
//	email package -key signing.key -out email.signed.json
//
// Secrets come from the Docker secrets the core hands the supervisor:
// togavsu_api_token, togavsu_webhook_secret, togavsu_db_role, togavsu_db_password and
// togavsu_db_schema. Mail credentials are never here: admins and mailbox owners enter
// them at runtime and they live in the core's module secret store.
//
// Packaging and signing go through the core's public module kit (pkg/modulekit): the
// package command holds back the parts of package/pending.yaml the pinned core refuses
// (modulekit.BuildInstallable), runs the core's install checks on the rest, packs and
// signs it, and prints each held-back part with the CORE item it waits on.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/bootstrap"
	"github.com/CharitonMedia/togavsu-email/internal/packaging"
)

func main() {
	log.SetFlags(zero)
	args := os.Args[one:]
	if len(args) < 1 {
		log.Fatal("usage: email serve|package [flags]")
	}
	switch args[0] {
	case "serve":
		serve(args[one:])
	case "package":
		pack(args[one:])
	default:
		log.Fatalf("unknown command %q", args[0])
	}
}

func secret(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		log.Fatalf("secret %s: %v", name, err)
	}
	return strings.TrimSpace(string(b))
}

func optional(dir, name, fallback string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return fallback
	}
	return strings.TrimSpace(string(b))
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", bootstrap.Env("TOGAVSU_LISTEN", ":8080"), "address to listen on")
	core := fs.String("core", bootstrap.Env("TOGAVSU_CORE_URL", "http://core:8080"), "core base URL inside the stack")
	dir := fs.String("secrets", "/run/secrets", "directory of Docker secrets")
	host := fs.String("db-host", bootstrap.Env("TOGAVSU_DB_HOST", "postgres:5432"), "Postgres host:port")
	name := fs.String("db-name", bootstrap.Env("TOGAVSU_DB_NAME", "togavsu"), "database name")
	_ = fs.Parse(args)
	schema := optional(*dir, "togavsu_db_schema", "mod_email")
	u := url.URL{Scheme: "postgres", User: url.UserPassword(secret(*dir, "togavsu_db_role"), secret(*dir, "togavsu_db_password")),
		Host: *host, Path: "/" + *name, RawQuery: url.Values{"search_path": {schema}}.Encode()}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		log.Fatal(err)
	}
	m := email.New(email.Config{Core: *core, Token: secret(*dir, "togavsu_api_token"),
		WebhookSecret: secret(*dir, "togavsu_webhook_secret"), DB: db,
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx)
	srv := &http.Server{Addr: *listen, Handler: m.Handler()}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("email listening on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func pack(args []string) {
	fs := flag.NewFlagSet("package", flag.ExitOnError)
	keyFile := fs.String("key", "", "author signing key file (a base64 Ed25519 seed)")
	out := fs.String("out", "email.signed.json", "signed package file to write")
	_ = fs.Parse(args)
	if *keyFile == "" {
		log.Fatal("package: -key is required")
	}
	b, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Fatal(err)
	}
	key, err := packaging.ParseKey(string(b))
	if err != nil {
		log.Fatal(err)
	}
	pkg, held, err := email.Build(key, nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, pkg, packageFileMode); err != nil {
		log.Fatal(err)
	}
	for _, h := range held {
		fmt.Fprintln(os.Stderr, h.String())
	}
	files, err := email.Files()
	if err != nil {
		log.Fatal(err)
	}
	unused, err := packaging.Unused(files, held)
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range unused {
		fmt.Fprintf(os.Stderr, "pending %s (%s) holds nothing back: the core accepts it or the package no longer has it\n", e.Part(), e.Core)
	}
	fmt.Printf("wrote %s\nsigning key (add it as a trusted key):\n%s", *out, packaging.PublicKeyPEM(key))
}
