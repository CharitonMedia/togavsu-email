// Package pkgfiles embeds the module package directory: the manifest, its migrations and
// the pending list (pending.yaml) of the parts that wait on core changes. The pending
// list never ships; modulekit.Installable leaves it out.
package pkgfiles

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed togavsu-module.yaml pending.yaml migrations/*.sql
var packageFS embed.FS

// manifestFile is the manifest's path in the package. This directory is the package
// itself, so its Go files carry no fixed.go of their own.
const manifestFile = "togavsu-module.yaml"

// Files returns the package directory's files by package path, pending.yaml included.
func Files() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(packageFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := packageFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[p] = b
		return nil
	})
	return out, err
}

// Migrations returns the migration files in order, by file name.
func Migrations() ([]string, map[string]string, error) {
	files, err := Files()
	if err != nil {
		return nil, nil, err
	}
	var names []string
	body := map[string]string{}
	for p, b := range files {
		if n, ok := strings.CutPrefix(p, "migrations/"); ok {
			names = append(names, n)
			body[n] = string(b)
		}
	}
	sort.Strings(names)
	return names, body, nil
}

// Defaults returns each setting's default as the manifest's settings list declares it;
// the core answers an unset setting with this value. It reads only that list, so it
// works on a manifest the pinned core's full validation still refuses.
func Defaults() (map[string]string, error) {
	b, err := packageFS.ReadFile(manifestFile)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	in, key := false, ""
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && !strings.HasPrefix(l, " ") {
			in = strings.HasPrefix(l, "settings:")
			continue
		}
		t := strings.TrimSpace(l)
		if !in {
			continue
		}
		if k, ok := strings.CutPrefix(t, "- key:"); ok {
			key = strings.TrimSpace(k)
			continue
		}
		if v, ok := strings.CutPrefix(t, "default:"); ok && key != "" {
			v = strings.TrimSpace(v)
			if strings.HasPrefix(v, `"`) {
				if v, err = strconv.Unquote(v); err != nil {
					return nil, fmt.Errorf("pkgfiles: default of %s: %w", key, err)
				}
			}
			out[key] = v
		}
	}
	return out, nil
}
