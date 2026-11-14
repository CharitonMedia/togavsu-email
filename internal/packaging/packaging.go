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

// Installable is what the pinned core installs (modulekit.Installable): the files
// without the pending list and without each pending part the core refuses (Files), the
// parts it held back (Held) and the pending entries that held nothing back (Unused),
// whose part the core accepts now or the package no longer has. A refusal no pending
// entry covers is an error.
func Installable(files map[string][]byte) (modulekit.InstallablePackage, error) {
	return modulekit.Installable(files)
}

// Build signs Installable's files with key as the signed package file the admin
// installs, and returns Installable's result. It refuses to sign while the core refuses
// a part pending.yaml doesn't cover.
func Build(key ed25519.PrivateKey, files map[string][]byte) ([]byte, modulekit.InstallablePackage, error) {
	return modulekit.BuildInstallable(key, files)
}

// ParseKey reads a signing key file's text: a base64 Ed25519 seed.
func ParseKey(text string) (ed25519.PrivateKey, error) { return modulekit.ParseSeed(text) }

// PublicKeyPEM is the PEM block the admin adds as the trusted signing key for key.
func PublicKeyPEM(key ed25519.PrivateKey) string {
	return modulekit.PublicKeyPEM(key.Public().(ed25519.PublicKey))
}
