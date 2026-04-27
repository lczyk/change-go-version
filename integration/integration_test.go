//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
	"github.com/lczyk/change-go-version/integration/internal/proxyspec"
	"golang.org/x/mod/module"
)

func TestMain(m *testing.M) {
	if _, err := os.Stat(sumsPath); os.IsNotExist(err) {
		println("integration: integration/proxy/SUMS missing — run `make proxy` first")
		os.Exit(1)
	}

	if err := materializeZips(); err != nil {
		println("materialize zips:", err.Error())
		os.Exit(1)
	}

	tmp, err := os.MkdirTemp("", "cgv-int-")
	if err != nil {
		println("mktemp:", err.Error())
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	binPath = filepath.Join(tmp, "change-go-version")
	build := exec.Command("go", "build", "-o", binPath, repoRoot)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		println("build cli:", err.Error())
		os.Exit(1)
	}

	modCache = filepath.Join(tmp, "modcache")
	if err := os.MkdirAll(modCache, 0o755); err != nil {
		println("mkdir modcache:", err.Error())
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// materializeZips ensures every entry in spec.txt has its .zip on disk in the
// proxy tree. Zips are gitignored — buildproxy commits only .info/.mod/list/
// SUMS, and tests rebuild zips deterministically via the same proxyspec code.
// Existing zips are reused (small enough that re-checking is cheap; we skip
// rebuilds when the file is non-empty).
func materializeZips() error {
	specPath := filepath.Join(repoRoot, "integration", "spec.txt")
	entries, err := proxyspec.Parse(specPath)
	if err != nil {
		return err
	}
	proxyRoot := filepath.Join(repoRoot, "integration", "proxy")
	for _, e := range entries {
		escaped, err := module.EscapePath(e.Mod)
		if err != nil {
			return err
		}
		base := filepath.Join(proxyRoot, filepath.FromSlash(escaped), "@v")
		zipPath := filepath.Join(base, e.Ver+".zip")
		if st, err := os.Stat(zipPath); err == nil && st.Size() > 0 {
			continue
		}
		modPath := filepath.Join(base, e.Ver+".mod")
		modContent, err := os.ReadFile(modPath)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		mv := module.Version{Path: e.Mod, Version: e.Ver}
		if err := proxyspec.BuildZip(&buf, mv, modContent, e.Pkgs); err != nil {
			return err
		}
		if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// go-flags release map (declared go directives):
//   v1.4.0 — none (treated as 1.0)
//   v1.5.0 — go 1.15
//   v1.6.0 — go 1.20
//   v1.6.1 — go 1.20

// TestSingleDirectDowngrade — target 1.15 → highest version with go ≤ 1.15
// is v1.5.0. --no-tidy keeps the assertion focused on the picker.
func TestSingleDirectDowngrade(t *testing.T) {
	dir := setupFixture(t, "single-direct")
	assertOK(t, runTool(t, dir, "--to", "1.15", "--no-tidy"))
	f := loadGoMod(t, dir)
	assert.Equal(t, f.Go.Version, "1.15")
	assert.Equal(t, requireVersion(t, f, "github.com/jessevdk/go-flags"), "v1.5.0")
}

// TestSingleDirectUpgrade — fixture pinned at v1.5.0 with go 1.15. Target 1.20
// raises ceiling, picker should walk up to v1.6.1.
func TestSingleDirectUpgrade(t *testing.T) {
	dir := setupFixture(t, "upgrade")
	assertOK(t, runTool(t, dir, "--to", "1.20", "--no-tidy"))
	f := loadGoMod(t, dir)
	assert.Equal(t, f.Go.Version, "1.20")
	assert.Equal(t, requireVersion(t, f, "github.com/jessevdk/go-flags"), "v1.6.1")
}

// TestUnresolvableDirect — every version of acme.test/needs-future declares
// go 99.99. Target 1.21 has no compatible version → tool exits non-zero and
// restores the original go.mod byte-for-byte.
func TestUnresolvableDirect(t *testing.T) {
	dir := setupFixture(t, "unresolvable")
	orig := readBytes(t, dir, "go.mod")
	assertFailed(t, runTool(t, dir, "--to", "1.21", "--no-tidy"))
	bytesEqual(t, readBytes(t, dir, "go.mod"), orig, "go.mod should be restored on failure")
}

// TestTidyKeepsRequire — runs without --no-tidy. Blank import keeps the dep
// "used" so tidy preserves the require even after the picker rewrites it.
func TestTidyKeepsRequire(t *testing.T) {
	dir := setupFixture(t, "single-direct")
	assertOK(t, runTool(t, dir, "--to", "1.15"))
	f := loadGoMod(t, dir)
	assert.Equal(t, requireVersion(t, f, "github.com/jessevdk/go-flags"), "v1.5.0")
}

// TestIndirectCascade — direct@v1.0.0 requires indirect@v1.5.0 (go 1.25).
// Target 1.20 forces a round-2 downgrade of the indirect to v1.0.0 (go 1.15).
func TestIndirectCascade(t *testing.T) {
	dir := setupFixture(t, "indirect")
	assertOK(t, runTool(t, dir, "--to", "1.20", "--no-tidy"))
	f := loadGoMod(t, dir)
	assert.Equal(t, f.Go.Version, "1.20")
	assert.Equal(t, requireVersion(t, f, "acme.test/indirect"), "v1.0.0")
}

// TestMissingGoMod — running against a directory with no go.mod must fail
// immediately with a clear error and not create one.
func TestMissingGoMod(t *testing.T) {
	dir := t.TempDir()
	assertFailed(t, runTool(t, dir, "--to", "1.20"))
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	assert.That(t, os.IsNotExist(err), "go.mod should not exist after failure, stat err=%v", err)
}

// TestAutoWalk — fixture starts at go 1.20. Check passes when the go.mod's
// minor is >= 18; fails below. Walk: 1.20 (baseline) → 1.19 → 1.18 → 1.17
// (fail) → break, lowest passing 1.18 is applied. Release-feed is stubbed via
// CGV_RELEASE_FEED so latestPatch returns -1 (no patches) and the patch arm
// short-circuits.
func TestAutoWalk(t *testing.T) {
	dir := setupFixture(t, "auto")

	// Stub release feed: empty list → latestPatch returns -1 → break outer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	check := `awk '$1=="go"{split($2,a,"."); exit (a[2]+0 >= 18) ? 0 : 1}' go.mod`
	assertOK(t, runToolEnv(t, dir,
		[]string{"CGV_RELEASE_FEED=" + srv.URL},
		"--auto", check,
	))
	f := loadGoMod(t, dir)
	assert.Equal(t, f.Go.Version, "1.18")
}

// TestRoundtrip — fixture mirrors the repo's own go.mod (go 1.21.0 + go-flags,
// lczyk/assert, lczyk/version/go, x/mod, x/sys-as-indirect).
//
// Asserts two invariants:
//
//  1. After --to 1.26 the picker walks every direct dep up to its highest
//     version whose declared go ≤ 1.26 (notably x/mod v0.20.0 → v0.22.0).
//  2. End state depends only on the final target, not the path taken: a fresh
//     --to 1.21.0 applied to the baseline produces the same go.mod and go.sum
//     bytes as --to 1.26 followed by --to 1.21.0.
//
// (1) confirms upgrade behaviour, (2) confirms the picker is path-independent
// for these inputs — the meaningful round-trip property given that `go get`
// is greedy and never voluntarily rolls a dep backwards on its own.
func TestRoundtrip(t *testing.T) {
	// Reference run: fresh fixture, single downgrade to 1.21.0.
	refDir := setupFixture(t, "real")
	assertOK(t, runTool(t, refDir, "--to", "1.21.0"))
	refMod := readBytes(t, refDir, "go.mod")
	refSum := readBytes(t, refDir, "go.sum")

	// Round-trip run: upgrade then downgrade.
	rtDir := setupFixture(t, "real")
	assertOK(t, runTool(t, rtDir, "--to", "1.26"))

	// Post-upgrade direct-dep snapshot.
	f := loadGoMod(t, rtDir)
	wantUp := map[string]string{
		"github.com/jessevdk/go-flags": "v1.6.1",
		"github.com/lczyk/assert":      "v0.3.1",
		"github.com/lczyk/version/go":  "v0.5.0",
		"golang.org/x/mod":             "v0.22.0",
	}
	for mod, want := range wantUp {
		assert.Equal(t, requireVersion(t, f, mod), want)
	}

	assertOK(t, runTool(t, rtDir, "--to", "1.21.0"))
	bytesEqual(t, readBytes(t, rtDir, "go.mod"), refMod, "go.mod after round-trip")
	bytesEqual(t, readBytes(t, rtDir, "go.sum"), refSum, "go.sum after round-trip")
}
