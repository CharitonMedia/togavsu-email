// Package kittest holds what the module's tests share on the core's public module kit:
// the package files as the pinned core installs them, and throwaway signing keys.
//
// The manifest declares fields and scopes the spec's core changes add, and
// package/pending.yaml names each with its CORE item. Installable is the kit's
// modulekit.Installable over the module's package: the full package once the pinned core
// accepts it, and until then the package without the pending parts the core refuses, so
// the suites drive everything else on the real core.
package kittest

import (
	"crypto/ed25519"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	email "github.com/CharitonMedia/togavsu-email"
)

// Files is the module's package directory's files, pending.yaml included.
func Files(t testing.TB) map[string][]byte {
	t.Helper()
	files, err := email.Files()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Installable is the package files the pinned core installs and the parts held back to
// get there. It fails the test on a refusal pending.yaml doesn't cover.
func Installable(t testing.TB) (map[string][]byte, []modulekit.Held) {
	t.Helper()
	files, held, err := modulekit.Installable(Files(t))
	if err != nil {
		t.Fatalf("the pinned core refuses parts pending.yaml doesn't cover: %v", err)
	}
	for _, h := range held {
		t.Log(h.String())
	}
	return files, held
}

// Key is a throwaway Ed25519 signing key made at test time.
func Key(t testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}
