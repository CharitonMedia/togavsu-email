// Package bootstrap is the module's only reader of the process environment, as the
// core's settings source check (modulekit.CheckSource) requires: the deployment's
// listen address, core URL and database host, and the test database URL. Everything a
// firm adjusts comes from the core's module settings instead.
package bootstrap

import "os"

// Env is the environment variable name, or fallback when it is unset or empty.
func Env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestDatabaseURL is the superuser Postgres URL the database tests use, or "" to skip
// them.
func TestDatabaseURL() string { return os.Getenv("TOGAVSU_TEST_DATABASE_URL") }
