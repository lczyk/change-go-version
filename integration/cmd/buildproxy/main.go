// buildproxy assembles a hermetic, source-free Go module proxy tree under -out
// from a spec file. It writes the small text artefacts that ship in the repo:
// .info, .mod, @v/list, and SUMS. The .zip files are NOT written here — they
// are gitignored and materialised lazily at test time from spec.txt + the
// committed .mod files via the same proxyspec.BuildZip routine, so the bytes
// served match the bytes hashed.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lczyk/change-go-version/integration/internal/proxyspec"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb/dirhash"
)

func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// stripGoMod parses an upstream go.mod and rewrites it keeping only `module`
// and `go` directives. Drops require, replace, exclude, retract, toolchain.
func stripGoMod(raw []byte, modPath string) ([]byte, error) {
	f, err := modfile.Parse("go.mod", raw, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range f.Require {
		_ = f.DropRequire(r.Mod.Path)
	}
	for _, r := range f.Replace {
		_ = f.DropReplace(r.Old.Path, r.Old.Version)
	}
	for _, r := range f.Exclude {
		_ = f.DropExclude(r.Mod.Path, r.Mod.Version)
	}
	f.Retract = nil
	f.DropToolchainStmt()
	if f.Module == nil {
		return nil, fmt.Errorf("go.mod missing module directive")
	}
	if f.Module.Mod.Path != modPath {
		f.Module.Mod.Path = modPath
	}
	f.Cleanup()
	return f.Format()
}

// hashGoMod computes the h1 of a go.mod file as Go's module system does for
// "$mod $ver/go.mod h1:..." entries.
func hashGoMod(modPath string) (string, error) {
	return dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return os.Open(modPath)
	})
}

// renderGoMod builds the go.mod content that will be served for an entry.
// Either fetched + stripped from upstream, or synthesised from -go=. Then
// any -req= entries are injected.
func renderGoMod(e proxyspec.Entry, escaped, proxy string) ([]byte, error) {
	var stripped []byte
	if e.GoStmt != "" {
		stripped = []byte(fmt.Sprintf("module %s\n\ngo %s\n", e.Mod, e.GoStmt))
	} else {
		raw, err := fetch(fmt.Sprintf("%s/%s/@v/%s.mod", proxy, escaped, e.Ver))
		if err != nil {
			return nil, fmt.Errorf("fetch %s@%s.mod: %w", e.Mod, e.Ver, err)
		}
		stripped, err = stripGoMod(raw, e.Mod)
		if err != nil {
			return nil, fmt.Errorf("strip %s@%s: %w", e.Mod, e.Ver, err)
		}
	}
	if len(e.Reqs) > 0 {
		f, err := modfile.Parse("go.mod", stripped, nil)
		if err != nil {
			return nil, fmt.Errorf("re-parse %s@%s for reqs: %w", e.Mod, e.Ver, err)
		}
		for _, r := range e.Reqs {
			if err := f.AddRequire(r.Path, r.Version); err != nil {
				return nil, fmt.Errorf("add require %s@%s -> %s@%s: %w", e.Mod, e.Ver, r.Path, r.Version, err)
			}
		}
		f.Cleanup()
		out, err := f.Format()
		if err != nil {
			return nil, fmt.Errorf("format %s@%s with reqs: %w", e.Mod, e.Ver, err)
		}
		stripped = out
	}
	return stripped, nil
}

func main() {
	specPath := flag.String("spec", "integration/spec.txt", "path to spec file")
	outDir := flag.String("out", "integration/proxy", "proxy output root")
	proxy := flag.String("proxy", "https://proxy.golang.org", "upstream proxy")
	flag.Parse()

	entries, err := proxyspec.Parse(*specPath)
	if err != nil {
		die("parse spec: %v", err)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		die("mkdir %s: %v", *outDir, err)
	}

	versionsByMod := map[string][]string{}
	var sumsLines []string

	for _, e := range entries {
		escaped, err := module.EscapePath(e.Mod)
		if err != nil {
			die("escape %s: %v", e.Mod, err)
		}
		base := filepath.Join(*outDir, filepath.FromSlash(escaped), "@v")
		if err := os.MkdirAll(base, 0o755); err != nil {
			die("mkdir %s: %v", base, err)
		}

		modContent, err := renderGoMod(e, escaped, *proxy)
		if err != nil {
			die("%v", err)
		}

		info := []byte(fmt.Sprintf(`{"Version":%q,"Time":"2000-01-01T00:00:00Z"}`+"\n", e.Ver))
		modPath := filepath.Join(base, e.Ver+".mod")
		infoPath := filepath.Join(base, e.Ver+".info")

		if err := os.WriteFile(modPath, modContent, 0o644); err != nil {
			die("write %s: %v", modPath, err)
		}
		if err := os.WriteFile(infoPath, info, 0o644); err != nil {
			die("write %s: %v", infoPath, err)
		}

		// Compute zip h1 in-memory; .zip itself is materialised lazily at test time.
		mv := module.Version{Path: e.Mod, Version: e.Ver}
		var buf bytes.Buffer
		if err := proxyspec.BuildZip(&buf, mv, modContent, e.Pkgs); err != nil {
			die("build zip %s@%s: %v", e.Mod, e.Ver, err)
		}
		zipH, err := proxyspec.HashZipBytes(buf.Bytes())
		if err != nil {
			die("hash zip %s@%s: %v", e.Mod, e.Ver, err)
		}
		modH, err := hashGoMod(modPath)
		if err != nil {
			die("hash go.mod %s: %v", modPath, err)
		}

		sumsLines = append(sumsLines,
			fmt.Sprintf("%s %s %s", e.Mod, e.Ver, zipH),
			fmt.Sprintf("%s %s/go.mod %s", e.Mod, e.Ver, modH),
		)
		versionsByMod[escaped] = append(versionsByMod[escaped], e.Ver)
		fmt.Printf("ok  %s %s\n", e.Mod, e.Ver)
	}

	for esc, vers := range versionsByMod {
		sort.Strings(vers)
		listPath := filepath.Join(*outDir, filepath.FromSlash(esc), "@v", "list")
		body := strings.Join(vers, "\n") + "\n"
		if err := os.WriteFile(listPath, []byte(body), 0o644); err != nil {
			die("write %s: %v", listPath, err)
		}
	}

	sort.Strings(sumsLines)
	sumsPath := filepath.Join(*outDir, "SUMS")
	if err := os.WriteFile(sumsPath, []byte(strings.Join(sumsLines, "\n")+"\n"), 0o644); err != nil {
		die("write %s: %v", sumsPath, err)
	}
	fmt.Printf("wrote %s (%d lines)\n", sumsPath, len(sumsLines))
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "buildproxy: "+format+"\n", a...)
	os.Exit(1)
}
