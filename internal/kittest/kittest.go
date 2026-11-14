// Package kittest holds what the module's tests share on the core's public module kit:
// the package files as the pinned core can install them, and throwaway signing keys.
//
// The manifest declares fields the spec's core changes add (container.system_mail,
// device_connect, notifications, and the sidebar's icon, order, section and badge). A
// core that lacks them refuses the whole manifest. Installable returns the package
// unchanged once the pinned core accepts it, and until then the package with exactly
// those fields taken out, naming each with its CORE item, so the suites can drive
// everything else on the real core. Only tests use it: the package command always
// ships the full manifest, and refuses to sign while the pinned core refuses it.
package kittest

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/CharitonMedia/togavsu/pkg/modulekit"

	email "github.com/CharitonMedia/togavsu-email"
)

// Pending is a manifest field the pinned core may lack, with the core change that adds
// it.
type Pending struct {
	Field string
	Core  string
}

// Fields lists the manifest fields this module declares from core changes.
var Fields = []Pending{
	{"container.system_mail", "CORE-05"},
	{"device_connect", "CORE-28"},
	{"notifications", "CORE-03"},
	{"sidebar.section, icon, order and badge", "CORE-08"},
}

// Items is the CORE items of Fields, comma-separated.
func Items() string {
	var s []string
	for _, f := range Fields {
		s = append(s, f.Core)
	}
	return strings.Join(s, ", ")
}

// Files is the module's package files.
func Files(t testing.TB) map[string][]byte {
	t.Helper()
	files, err := email.Files()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Installable is the package files the pinned core installs, and whether they are the
// full package. When the core refuses the full manifest, the fields of Fields are taken
// out and the reduced package must pass the core's checks.
func Installable(t testing.TB) (map[string][]byte, bool) {
	t.Helper()
	files := Files(t)
	if _, _, err := modulekit.Check(files); err == nil {
		return files, true
	}
	reduced := Reduce(files)
	if _, _, err := modulekit.Check(reduced); err != nil {
		t.Fatalf("the package without the core-change fields is refused too: %v", err)
	}
	t.Logf("the pinned core refuses the full manifest; installing it without %s (%s)", fieldNames(), Items())
	return reduced, false
}

func fieldNames() string {
	var s []string
	for _, f := range Fields {
		s = append(s, f.Field)
	}
	return strings.Join(s, "; ")
}

// Reduce returns files with the manifest's core-change fields taken out.
func Reduce(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for k, v := range files {
		out[k] = v
	}
	lines := strings.Split(string(files[modulekit.ManifestFile]), "\n")
	var kept []string
	top := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		ind := indent(l)
		if ind == 0 && strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "#") {
			top, _, _ = strings.Cut(l, ":")
		}
		key := strings.TrimPrefix(strings.TrimSpace(l), "- ")
		key, _, _ = strings.Cut(key, ":")
		drop := false
		switch {
		case ind == 0 && (top == "device_connect" || top == "notifications"):
			drop = true
		case top == "container" && ind > 0 && key == "system_mail":
			drop = true
		case top == "sidebar" && ind > 0 && !strings.HasPrefix(strings.TrimSpace(l), "- ") &&
			(key == "section" || key == "subgroup" || key == "icon" || key == "order" || key == "badge"):
			drop = true
		}
		if !drop {
			kept = append(kept, l)
			continue
		}
		// Skip the field and everything indented under it.
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" && indent(lines[i+1]) > ind {
			i++
		}
	}
	out[modulekit.ManifestFile] = []byte(strings.Join(kept, "\n"))
	return out
}

func indent(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// Key is a throwaway Ed25519 signing key made at test time.
func Key(t testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}
