package packaging_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	email "github.com/CharitonMedia/togavsu-email"
	"github.com/CharitonMedia/togavsu-email/internal/kittest"
	"github.com/CharitonMedia/togavsu-email/internal/packaging"
)

// The package directory on disk and the embedded package are the same files. The
// pinned core's install checks refuse the full manifest, and they refuse it for each
// pending field of kittest.Fields on its own: with every other pending field taken out,
// each "check" field alone is still refused. Once the core accepts one, this fails and
// names it, so the field leaves the list and the CLI check returns to CI.
func TestPackageDirRefusedOnlyForPendingFields(t *testing.T) {
	disk, err := modulekit.ReadDir("../../package")
	if err != nil {
		t.Fatal(err)
	}
	delete(disk, "files.go") // the embedding Go file, not a package file
	embedded := kittest.Files(t)
	if len(disk) != len(embedded) {
		t.Fatalf("disk %d files, embedded %d", len(disk), len(embedded))
	}
	for p, b := range embedded {
		if string(disk[p]) != string(b) {
			t.Fatalf("%s differs between the package directory and the embedded package", p)
		}
	}
	if _, _, err := modulekit.Check(disk); err == nil {
		t.Fatal("the pinned core accepts the full manifest: empty kittest.Fields and restore the togavsu-module check step in ci.yml")
	}
	for _, f := range kittest.Fields {
		_, _, err := modulekit.Check(kittest.Only(disk, f))
		switch {
		case f.Stage == "check" && err == nil:
			t.Errorf("the pinned core now accepts %s (%s): take it out of kittest.Fields", f.Field, f.Core)
		case f.Stage == "approve" && err != nil:
			t.Errorf("%s (%s) is refused at install, not at approval: %v", f.Field, f.Core, err)
		}
	}
	raw, err := modulekit.Archive(kittest.Reduce(disk))
	if err != nil {
		t.Fatal(err)
	}
	pkg, ms, err := modulekit.CheckPackage(raw)
	if err != nil || pkg.Manifest.ID != "email" || len(ms) != 5 {
		t.Fatalf("CheckPackage: %v %+v %d", err, pkg, len(ms))
	}
}

// The package without the core-change fields passes the pinned core's checks, so the
// only refusals are those fields.
func TestReducedPackagePassesTheCoreChecks(t *testing.T) {
	files := kittest.Reduce(kittest.Files(t))
	pkg, ms, err := modulekit.Check(files)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Manifest.ID != "email" || pkg.Manifest.Version != "1.0.0" || len(ms) != 5 {
		t.Fatalf("%+v, %d migrations", pkg.Manifest, len(ms))
	}
	for _, f := range []string{"system_mail:", "device_connect:", "\nnotifications:", "badge:", "icon:"} {
		if strings.Contains(string(files[modulekit.ManifestFile]), f) {
			t.Fatalf("reduced manifest still has %s", f)
		}
	}
}

// Built and signed with a throwaway key, the signed package file passes the core's own
// verification; a tampered file and an untrusted signer are refused with the codes the
// install API answers.
func TestSignedPackageVerifies(t *testing.T) {
	files, full := kittest.Installable(t)
	pub, priv := kittest.Key(t)
	signed, err := packaging.Sign(priv, files)
	if err != nil {
		t.Fatal(err)
	}
	v, ms, err := modulekit.VerifyFile(signed, []modulekit.TrustedKey{modulekit.Trust(pub)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Manifest.ID != "email" || len(ms) != 5 {
		t.Fatalf("verified %+v, %d migrations", v.Manifest, len(ms))
	}
	if full {
		if again, err := email.Build(priv, nil); err != nil || string(again) != string(signed) {
			t.Fatalf("email.Build gives other bytes: %v", err)
		}
	}

	// One byte of the package changed under the same signature.
	var sp modulekit.SignedPackage
	if err := json.Unmarshal(signed, &sp); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(sp.PackageBase64)
	if err != nil {
		t.Fatal(err)
	}
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

// The package command's path refuses to sign a package the core's checks refuse, as the
// kit's own package command does.
func TestSignRefusesWhatTheCoreRefuses(t *testing.T) {
	_, priv := kittest.Key(t)
	files := kittest.Reduce(kittest.Files(t))
	files["migrations/0006_bad.sql"] = []byte("CREATE TABLE core.stolen (id int);\n")
	if _, err := packaging.Sign(priv, files); modulekit.Code(err) != modulekit.CodeMigration {
		t.Fatalf("signed a refused package: %v", err)
	}
	if _, err := packaging.Archive(files); err == nil {
		t.Fatal("archived a refused package")
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
