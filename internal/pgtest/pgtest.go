// Package pgtest gives tests a Postgres schema with the module's migrations applied.
// It uses only the togavsu_email database: a stand-in core schema holds bare core record
// ids (so the module's ON DELETE RESTRICT foreign keys have something to reference) and
// each test gets its own mod_email_<random> schema, dropped when the test ends.
//
// The server comes from TOGAVSU_TEST_DATABASE_URL (any database on it; the helper
// creates togavsu_email beside it). Without it the tests that need Postgres skip.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu-email/internal/bootstrap"
	"github.com/CharitonMedia/togavsu-email/internal/ids"
	pkgfiles "github.com/CharitonMedia/togavsu-email/package"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Database is the only database the tests create and use.
const Database = "togavsu_email"

// coreTables are the core records the module links to.
var coreTables = []string{"users", "matters", "contacts", "files", "notes"}

// DB is one test's schema.
type DB struct {
	// Module is a connection whose search_path is the test's module schema.
	Module *sql.DB
	// Core reaches the stand-in core schema (the fake core writes record ids there).
	Core   *sql.DB
	Schema string
}

func baseURL(t testing.TB) *url.URL {
	raw := bootstrap.TestDatabaseURL()
	if raw == "" {
		t.Skip("TOGAVSU_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// New makes a fresh module schema with the migrations applied.
func New(t testing.TB) *DB {
	t.Helper()
	ctx := context.Background()
	u := baseURL(t)
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, Database).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+Database); err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Fatal(err)
		}
	}
	du := *u
	du.Path = "/" + Database
	core, err := sql.Open("pgx", du.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	tx, err := core.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(424242)`); err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE SCHEMA IF NOT EXISTS core`}
	for _, tb := range coreTables {
		stmts = append(stmts, `CREATE TABLE IF NOT EXISTS core.`+tb+` (id TEXT PRIMARY KEY)`)
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	schema := "mod_email_" + strings.ReplaceAll(ids.Secret(schemaSuffixBytes), "-", "")
	if _, err := core.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	mu := du
	q := mu.Query()
	q.Set("search_path", schema)
	mu.RawQuery = q.Encode()
	mod, err := sql.Open("pgx", mu.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mod.Close()
		_ = serial(context.Background(), core, func(c *sql.Conn) error {
			_, err := c.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
			return err
		})
	})
	names, bodies, err := pkgfiles.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	err = serial(ctx, mod, func(c *sql.Conn) error {
		for _, n := range names {
			if _, err := c.ExecContext(ctx, bodies[n]); err != nil {
				return fmt.Errorf("migration %s: %w", n, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &DB{Module: mod, Core: core, Schema: schema}
}

// serial runs f on one connection holding the schema lock: creating and dropping the
// module's foreign keys locks the shared core tables, so test packages running at the
// same time take turns rather than deadlock.
func serial(ctx context.Context, db *sql.DB, f func(*sql.Conn) error) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `SELECT pg_advisory_lock(424243)`); err != nil {
		return err
	}
	defer c.ExecContext(context.Background(), `SELECT pg_advisory_unlock(424243)`)
	return f(c)
}

// Record inserts a core record id into the stand-in core table.
func (d *DB) Record(t testing.TB, table, id string) {
	t.Helper()
	if err := d.Insert(context.Background(), table, id); err != nil {
		t.Fatal(err)
	}
}

// Insert inserts a core record id; the fake core uses it.
func (d *DB) Insert(ctx context.Context, table, id string) error {
	ok := false
	for _, tb := range coreTables {
		ok = ok || tb == table
	}
	if !ok {
		return fmt.Errorf("pgtest: no core table %s", table)
	}
	_, err := d.Core.ExecContext(ctx, `INSERT INTO core.`+table+` (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
	return err
}
