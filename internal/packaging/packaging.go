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

// Signer packs the package files (manifest and migrations by package path) and returns
// the marketplace's signed package file.
type Signer interface {
	SignedPackage(priv ed25519.PrivateKey, files map[string][]byte) ([]byte, error)
}

// Unavailable is the stub Signer.
type Unavailable struct{}

// SignedPackage implements Signer.
func (Unavailable) SignedPackage(ed25519.PrivateKey, map[string][]byte) ([]byte, error) {
	return nil, ErrUnavailable
}

// Default is the Signer the package command uses.
var Default Signer = Unavailable{}
