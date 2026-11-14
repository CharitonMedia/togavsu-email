// Package pkgsign packs and signs the module package the way the core's marketplace
// signing path does (marketplace.Sign, Digest, KeyID and SignedPackage in core), so the
// core verifies this module's signed package file exactly as it verifies matter-clock's.
// The module can't import the core's internal packages, so the few functions it needs
// are written here with the same encodings.
package pkgsign

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"sort"
)

// fileMode is the core's limits.json file_mode (0644).
const fileMode = 0o644

// KeyID is the fingerprint the marketplace and the core use to name pub.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "ed25519-" + hex.EncodeToString(sum[:])[:sha256.Size]
}

// PublicKeyPEM encodes pub as a PKIX "PUBLIC KEY" block.
func PublicKeyPEM(pub ed25519.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// Digest is the package digest, sha256:<hex>.
func Digest(pkg []byte) string {
	sum := sha256.Sum256(pkg)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Sign returns the base64 Ed25519 signature of pkg.
func Sign(priv ed25519.PrivateKey, pkg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, pkg))
}

// Verify checks a base64 signature of pkg against pub.
func Verify(pub ed25519.PublicKey, pkg []byte, signature string) error {
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, pkg, sig) {
		return errors.New("signature_invalid")
	}
	return nil
}

// SignedPackage is the marketplace's signed package file.
type SignedPackage struct {
	ListingID     string         `json:"listing_id"`
	Version       string         `json:"version"`
	Immutable     bool           `json:"immutable"`
	Manifest      map[string]any `json:"manifest"`
	Digest        string         `json:"digest"`
	Signature     string         `json:"signature"`
	KeyID         string         `json:"key_id"`
	PackageBase64 string         `json:"package_base64"`
}

// Archive writes files as a gzip tar in path order, as the core's modules.Archive does.
func Archive(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: fileMode, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[n]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack reads a gzip tar back into its files (tests use it to check a package).
func Unpack(pkg []byte) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(pkg))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[h.Name] = b
	}
}

// SignFile wraps package bytes in a signed package file for listing id and version.
func SignFile(priv ed25519.PrivateKey, id, version string, pkg []byte) ([]byte, error) {
	return json.Marshal(SignedPackage{ListingID: id, Version: version, Digest: Digest(pkg),
		Signature: Sign(priv, pkg), KeyID: KeyID(priv.Public().(ed25519.PublicKey)),
		PackageBase64: base64.StdEncoding.EncodeToString(pkg)})
}
