package packaging_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/kittest"
	"github.com/CharitonMedia/togavsu-email/internal/packaging"
)

// The package directory on disk and the embedded package are the same files, the
// pending list included.
func TestPackageDirIsTheEmbeddedPackage(t *testing.T) {
	disk, err := modulekit.ReadDir("../../package")
	if err != nil {
		t.Fatal(err)
	}
	delete(disk, "files.go") // the embedding Go file, not a package file
	embedded := kittest.Files(t)
	if _, ok := embedded[modulekit.PendingFile]; !ok {
		t.Fatal("the embedded package has no pending.yaml")
	}
	if len(disk) != len(embedded) {
		t.Fatalf("disk %d files, embedded %d", len(disk), len(embedded))
	}
	for p, b := range embedded {
		if string(disk[p]) != string(b) {
			t.Fatalf("%s differs between the package directory and the embedded package", p)
		}
	}
}

// The package builds and signs with a key made at runtime, holding back exactly the
// parts pending.yaml lists: the signed file verifies, its files pass the core's install
// checks with no refusals, every refusal of the full package is one a pending entry held
// back, and every pending entry held something back. Once the pinned core accepts a part,
// its entry holds nothing back and this fails naming it, so the entry leaves the list.
func TestBuildSignsTheInstallablePackage(t *testing.T) {
	files := kittest.Files(t)
	pub, priv := kittest.Key(t)
	signed, ip, err := packaging.Build(priv, files)
	if err != nil {
		t.Fatalf("Build with a valid key: %v", err)
	}

	v, ms, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)})
	if err != nil {
		t.Fatal(err)
	}
	if v.ModuleID != email.ID || v.Version != "1.0.0" || len(ms) != 5 {
		t.Fatalf("verified %s %s, %d migrations", v.ModuleID, v.Version, len(ms))
	}
	pkg, err := modulekit.ReadPackage(packageBytes(t, signed))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pkg.Files[modulekit.PendingFile]; ok {
		t.Fatal("the signed package ships pending.yaml")
	}
	if refs := modulekit.CheckAll(pkg.Files); len(refs) != 0 {
		t.Fatalf("the signed files are refused: %v", refs)
	}
	if _, _, err := modulekit.Check(pkg.Files); err != nil {
		t.Fatal(err)
	}

	// The same bytes come from the module's own Build and from the kit's.
	if again, _, err := email.Build(priv, nil); err != nil || string(again) != string(signed) {
		t.Fatalf("email.Build gives other bytes: %v", err)
	}

	for _, e := range ip.Unused {
		t.Errorf("pending %s (%s) holds nothing back: the pinned core accepts it or the package no longer has it; take it out of package/pending.yaml", e.Part(), e.Core)
	}

	full := make(map[string][]byte, len(files))
	for k, b := range files {
		if k != modulekit.PendingFile {
			full[k] = b
		}
	}
	covered := map[string]bool{}
	for _, h := range ip.Held {
		if len(h.Refusals) == 0 {
			t.Errorf("%s held back with no refusal", h)
		}
		for _, r := range h.Refusals {
			covered[r.Where()+" "+r.Code] = true
		}
	}
	for _, r := range modulekit.CheckAll(full) {
		if !covered[r.Where()+" "+r.Code] {
			t.Errorf("the pinned core refuses %s (%s) and no pending entry holds it back", r.Where(), r.Code)
		}
	}
}

// A pending list that misses a refusal is refused, and so is one that names a part the
// package doesn't have: the first fails Build, the second shows in Unused.
func TestPendingListMustMatchTheRefusals(t *testing.T) {
	_, priv := kittest.Key(t)
	files := kittest.Files(t)
	entries, err := modulekit.ParsePending(files[modulekit.PendingFile])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Skip("nothing is pending")
	}

	missing := copyFiles(files)
	missing[modulekit.PendingFile] = pendingYAML(entries[1:])
	_, _, err = packaging.Build(priv, missing)
	var refs modulekit.Refusals
	if !errors.As(err, &refs) || len(refs) == 0 {
		t.Fatalf("built without the entry for %s: %v", entries[0].Part(), err)
	}

	extra := copyFiles(files)
	stale := modulekit.Pending{Field: "sidebar[*].subgroup", Core: modulekit.CoreItems{"CORE-08"}, Reason: "A part this package doesn't have."}
	extra[modulekit.PendingFile] = pendingYAML(append(append([]modulekit.Pending(nil), entries...), stale))
	_, ip, err := packaging.Build(priv, extra)
	if err != nil {
		t.Fatal(err)
	}
	if len(ip.Unused) != 1 || ip.Unused[0].Part() != stale.Part() {
		t.Fatalf("unused %+v", ip.Unused)
	}
}

// One byte of the signed package changed under the same signature, and a signer the
// admin doesn't trust, are refused with the codes the install API answers.
func TestSignedPackageRefusesTamperAndUntrustedSigner(t *testing.T) {
	pub, priv := kittest.Key(t)
	signed, _, err := packaging.Build(priv, kittest.Files(t))
	if err != nil {
		t.Fatal(err)
	}
	var sp modulekit.SignedPackage
	if err := json.Unmarshal(signed, &sp); err != nil {
		t.Fatal(err)
	}
	raw := packageBytes(t, signed)
	raw[len(raw)/2] ^= 0xff
	sp.PackageBase64 = base64.StdEncoding.EncodeToString(raw)
	tampered, _ := json.Marshal(sp)
	if _, _, err := modulekit.VerifyFile(tampered, []modulekit.TrustedKey{modulekit.Trust(pub)}); modulekit.Code(err) != modulekit.CodeTampered {
		t.Fatalf("tampered package: %v (code %q)", err, modulekit.Code(err))
	}
	other, _ := kittest.Key(t)
	if _, _, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(other)}); err == nil {
		t.Fatal("an untrusted signer verified")
	}
}

// Build refuses to sign a package with a refusal no pending entry covers, as the kit's
// own package command does.
func TestBuildRefusesWhatTheCoreRefuses(t *testing.T) {
	_, priv := kittest.Key(t)
	files := kittest.Files(t)
	files["migrations/0006_bad.sql"] = []byte("CREATE TABLE core.stolen (id int);\n")
	_, _, err := packaging.Build(priv, files)
	var refs modulekit.Refusals
	if !errors.As(err, &refs) || refs[0].Kind != modulekit.KindMigration || refs[0].File != "migrations/0006_bad.sql" {
		t.Fatalf("signed a refused package: %v", err)
	}
	if _, err := email.Archive(func(f map[string][]byte) { f["migrations/0006_bad.sql"] = files["migrations/0006_bad.sql"] }); err == nil {
		t.Fatal("archived a refused package")
	}
	// The kit's Build won't sign the package directory with its pending list in it.
	if _, err := modulekit.Build(priv, kittest.Files(t)); !errors.As(err, &refs) || refs[0].Code != modulekit.RefusePendingUnbuilt {
		t.Fatalf("modulekit.Build on the pending package: %v", err)
	}
}

// The key file holds a base64 seed; anything else is refused.
func TestParseKey(t *testing.T) {
	_, priv := kittest.Key(t)
	k, err := packaging.ParseKey(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
	if err != nil || !k.Equal(priv) {
		t.Fatalf("seed: %v", err)
	}
	if !strings.Contains(packaging.PublicKeyPEM(k), "PUBLIC KEY") {
		t.Fatal("no PEM block")
	}
	if _, err := packaging.ParseKey("not a key"); err == nil {
		t.Fatal("accepted a bad key file")
	}
}

// The core's settings source check (AC-030) passes over the whole module root,
// test-support packages included: only internal/bootstrap reads the environment, and
// every fixed number is a named constant in its package's fixed.go.
func TestModuleSourceSettingsCheck(t *testing.T) {
	if err := modulekit.CheckSource("../.."); err != nil {
		t.Fatal(err)
	}
}

// packageBytes is the package archive inside a signed package file.
func packageBytes(t *testing.T, signed []byte) []byte {
	t.Helper()
	var sp modulekit.SignedPackage
	if err := json.Unmarshal(signed, &sp); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func copyFiles(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for k, v := range files {
		out[k] = v
	}
	return out
}

// pendingYAML writes entries as a pending list.
func pendingYAML(entries []modulekit.Pending) []byte {
	var b strings.Builder
	b.WriteString("pending:\n")
	for _, e := range entries {
		switch {
		case e.Field != "":
			b.WriteString("  - field: " + e.Field + "\n")
		case e.Scope != "":
			b.WriteString("  - scope: " + e.Scope + "\n")
		default:
			b.WriteString("  - migration: " + e.Migration + "\n")
		}
		b.WriteString("    core: [" + strings.Join(e.Core, ", ") + "]\n    reason: " + e.Reason + "\n")
	}
	return []byte(b.String())
}
