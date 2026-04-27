// Package proxyspec parses the buildproxy spec file and renders module zips
// deterministically. Both cmd/buildproxy (which computes h1 hashes ahead of
// time) and the integration test harness (which materialises zips lazily on
// disk) share this code so that the bytes hashed match the bytes served.
package proxyspec

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
)

// Entry is one line of spec.txt expanded into structured form.
type Entry struct {
	Mod, Ver string
	// GoStmt, if non-empty, makes the consumer synthesise a fresh go.mod
	// (`module $Mod\ngo $GoStmt\n`) instead of reading an upstream one.
	GoStmt string
	// Reqs are require lines to inject into the served go.mod after stripping
	// or synthesis. Drives indirect-cascade tests.
	Reqs []module.Version
	Pkgs []PkgSpec
}

// PkgSpec describes a stub package to embed in a module zip. Subpath "" or "."
// is the module root.
type PkgSpec struct {
	Subpath, Name string
}

// ParsePkg turns a spec token like "sub/pkg" or "sub/pkg=name" into a PkgSpec.
// Without an "=name" override the identifier is derived from the path tail.
func ParsePkg(tok string) PkgSpec {
	sub, name, ok := strings.Cut(tok, "=")
	if sub == "." {
		sub = ""
	}
	if !ok {
		tail := sub
		if i := strings.LastIndex(sub, "/"); i >= 0 {
			tail = sub[i+1:]
		}
		if tail == "" {
			tail = "root"
		}
		name = sanitizePkgName(tail)
	}
	return PkgSpec{Subpath: sub, Name: name}
}

func sanitizePkgName(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "pkg"
	}
	return out
}

// Parse reads a spec file from path. Lines: `mod ver [-go=X.Y] [-req=mod@ver] [pkg ...]`.
// Blank lines and `#` comments are ignored.
func Parse(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []Entry
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s:%d: need at least mod and ver", path, ln)
		}
		e := Entry{Mod: fields[0], Ver: fields[1]}
		for _, tok := range fields[2:] {
			if v, ok := strings.CutPrefix(tok, "-go="); ok {
				e.GoStmt = v
				continue
			}
			if v, ok := strings.CutPrefix(tok, "-req="); ok {
				modPath, ver, ok2 := strings.Cut(v, "@")
				if !ok2 {
					return nil, fmt.Errorf("%s:%d: -req=mod@ver expected, got %q", path, ln, tok)
				}
				e.Reqs = append(e.Reqs, module.Version{Path: modPath, Version: ver})
				continue
			}
			e.Pkgs = append(e.Pkgs, ParsePkg(tok))
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// BuildZip writes a module zip containing the given go.mod plus one
// `package <Name>` stub per PkgSpec. Output is deterministic for fixed inputs.
func BuildZip(w io.Writer, mv module.Version, goMod []byte, pkgs []PkgSpec) error {
	zw := zip.NewWriter(w)
	prefix := mv.Path + "@" + mv.Version + "/"

	add := func(name string, data []byte) error {
		fw, err := zw.Create(prefix + name)
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := add("go.mod", goMod); err != nil {
		return err
	}
	for _, p := range pkgs {
		dest := "stub.go"
		if p.Subpath != "" {
			dest = pathpkg.Join(p.Subpath, "stub.go")
		}
		body := []byte("package " + p.Name + "\n")
		if err := add(dest, body); err != nil {
			return err
		}
	}
	return zw.Close()
}

// HashZipBytes computes the h1 hash of a zip held in memory. Mirrors what
// dirhash.HashZip does for a zip on disk; ignores envelope metadata.
func HashZipBytes(zipBytes []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return "", err
	}
	files := make([]string, 0, len(r.File))
	zfiles := map[string]*zip.File{}
	for _, f := range r.File {
		files = append(files, f.Name)
		zfiles[f.Name] = f
	}
	return dirhash.Hash1(files, func(name string) (io.ReadCloser, error) {
		return zfiles[name].Open()
	})
}
