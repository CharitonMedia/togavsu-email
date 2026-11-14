// Package packaging is the seam where the module's package is packed and signed for the
// marketplace. The core's packing and signing code is internal to the core and is not
// copied here, so what the core verifies can't drift from what this module produces.
// Until the core publishes a public signing and packaging package or CLI, the only
// implementation is Unavailable, which refuses with ErrUnavailable.
package packaging

import (
	"crypto/ed25519"
	"errors"
)

// ErrUnavailable says packing and signing wait on the core's public tooling.
var ErrUnavailable = errors.New("packaging: waits on a public signing and packaging package or CLI from the core")

// Packager packs the package files (manifest and migrations by package path) and signs
// them as the marketplace's signed package file.
type Packager interface {
	// Archive packs the files as the unsigned package.
	Archive(files map[string][]byte) ([]byte, error)
	// SignedPackage packs and signs with the Ed25519 key from seed.
	SignedPackage(seed []byte, files map[string][]byte) ([]byte, error)
	// PublicKeyPEM is the public key for seed, for the admin's trusted keys.
	PublicKeyPEM(seed []byte) (string, error)
}

// Unavailable is the stub Packager.
type Unavailable struct{}

// Archive implements Packager.
func (Unavailable) Archive(map[string][]byte) ([]byte, error) { return nil, ErrUnavailable }

// SignedPackage implements Packager.
func (Unavailable) SignedPackage([]byte, map[string][]byte) ([]byte, error) {
	return nil, ErrUnavailable
}

// PublicKeyPEM implements Packager.
func (Unavailable) PublicKeyPEM(seed []byte) (string, error) {
	if len(seed) != ed25519.SeedSize {
		return "", errors.New("packaging: the key is a 32-byte Ed25519 seed")
	}
	return "", ErrUnavailable
}

// Default is the Packager the package command and email.Build use.
var Default Packager = Unavailable{}
