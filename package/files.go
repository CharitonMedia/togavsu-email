// Package pkgfiles embeds the module package: the manifest and its migrations.
package pkgfiles

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed togavsu-module.yaml migrations/*.sql
var packageFS embed.FS

// Files returns the package files by package path.
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
	sortStrings(names)
	return names, body, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
