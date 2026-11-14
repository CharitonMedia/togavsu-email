// Package packaging packs and signs the module's package through the core's public
// module kit (pkg/modulekit), so the bytes this module ships pass exactly the checks the
// core runs at install.
package packaging

import (
	"crypto/ed25519"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"
)

// Check runs the core's install checks (manifest, migration files, migration rules) on
// the package files.
func Check(files map[string][]byte) error {
	_, _, err := modulekit.Check(files)
	return err
}

// Archive packs the files as the gzip tar the core installs. It refuses files the
// core's install checks refuse.
func Archive(files map[string][]byte) ([]byte, error) {
	if err := Check(files); err != nil {
		return nil, err
	}
	return modulekit.Archive(files)
}

// Sign packs the files and signs them with key as the signed package file the admin
// installs. Like the kit's package command, it refuses to sign a package the install
// checks refuse.
func Sign(key ed25519.PrivateKey, files map[string][]byte) ([]byte, error) {
	if err := Check(files); err != nil {
		return nil, err
	}
	return modulekit.Build(key, files)
}

// ParseKey reads a signing key file's text: a base64 Ed25519 seed.
func ParseKey(text string) (ed25519.PrivateKey, error) { return modulekit.ParseSeed(text) }

// PublicKeyPEM is the PEM block the admin adds as the trusted signing key for key.
func PublicKeyPEM(key ed25519.PrivateKey) string {
	return modulekit.PublicKeyPEM(key.Public().(ed25519.PublicKey))
}
