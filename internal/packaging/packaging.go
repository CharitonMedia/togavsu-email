// Package packaging packs and signs the module's package through the core's public
// module kit (pkg/modulekit), so the bytes this module ships pass exactly the checks the
// core runs at install.
//
// The package directory keeps every part the spec's core changes add, and
// package/pending.yaml names each one with its CORE item. Build holds back the parts the
// pinned core refuses (modulekit.BuildInstallable) and signs the rest; a part the core
// accepts is never held back, so each returns on its own once its CORE item lands.
package packaging

import (
	"crypto/ed25519"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// Installable is the package files the pinned core installs: files without the pending
// list and without each pending part the core refuses, and the parts it held back. A
// refusal no pending entry covers is an error.
func Installable(files map[string][]byte) (map[string][]byte, []modulekit.Held, error) {
	return modulekit.Installable(files)
}

// Build signs Installable's files with key as the signed package file the admin
// installs, and returns the parts it held back. It refuses to sign while the core
// refuses a part pending.yaml doesn't cover.
func Build(key ed25519.PrivateKey, files map[string][]byte) ([]byte, []modulekit.Held, error) {
	return modulekit.BuildInstallable(key, files)
}

// Unused lists the pending entries of files' pending list that held is missing: the core
// accepts the part now, or the package no longer has it, so the entry can go.
func Unused(files map[string][]byte, held []modulekit.Held) ([]modulekit.Pending, error) {
	src, ok := files[modulekit.PendingFile]
	if !ok {
		return nil, nil
	}
	entries, err := modulekit.ParsePending(src)
	if err != nil {
		return nil, err
	}
	return modulekit.Unused(entries, held), nil
}

// ParseKey reads a signing key file's text: a base64 Ed25519 seed.
func ParseKey(text string) (ed25519.PrivateKey, error) { return modulekit.ParseSeed(text) }

// PublicKeyPEM is the PEM block the admin adds as the trusted signing key for key.
func PublicKeyPEM(key ed25519.PrivateKey) string {
	return modulekit.PublicKeyPEM(key.Public().(ed25519.PublicKey))
}
