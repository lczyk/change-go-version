package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
)

// fakeFeed serves a Go release JSON feed with the given versions and sets
// CGV_RELEASE_FEED to point at it for the duration of the test. Also resets
// patchCache so prior cached lookups don't bleed in.
func fakeFeed(t *testing.T, versions ...string) {
	t.Helper()
	body := "["
	for i, v := range versions {
		if i > 0 {
			body += ","
		}
		body += `{"version":"` + v + `"}`
	}
	body += "]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	prev, hadPrev := os.LookupEnv("CGV_RELEASE_FEED")
	os.Setenv("CGV_RELEASE_FEED", srv.URL)
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("CGV_RELEASE_FEED", prev)
		} else {
			os.Unsetenv("CGV_RELEASE_FEED")
		}
	})

	patchCache.Lock()
	patchCache.m = map[int]int{}
	patchCache.Unlock()
	t.Cleanup(func() {
		patchCache.Lock()
		patchCache.m = map[int]int{}
		patchCache.Unlock()
	})
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	assert.NoError(t, err)
	assert.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestCanonGoVersion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"1.22", "v1.22.0"},
		{"1.22.3", "v1.22.3"},
		{"1.22.0", "v1.22.0"},
		{"go1.22.3", "v1.22.3"},
		{"1.22rc1", "v1.22.0"},
		{"1.22-rc1", "v1.22.0"},
		{"v1.22.3", "v1.22.3"},
		{"", "v0.0.0"},
		{"1", "v1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, canonGoVersion(tc.in), tc.want)
		})
	}
}

func TestCompareGo(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// Go's toolchain ordering: language version 1.23 < 1.23rc1 < 1.23.0.
		{"1.22", "1.22.0", -1}, // bare minor sorts below the .0 release
		{"1.21", "1.22", -1},
		{"1.22.1", "1.22", 1},
		{"1.22", "1.22.1", -1},
		{"2.0", "1.99.99", 1},
		{"1.22.3", "1.22.3", 0},
		{"go1.22", "1.22.0", -1}, // prefix stripped, still below .0
		{"1.22rc1", "1.22", 1},   // rc sorts above the language version
		{"1.22", "1.22rc1", -1},
		{"1.22rc1", "1.22.0", -1}, // rc sorts below the release
	}
	for _, tc := range cases {
		assert.Equal(t, compareGo(tc.a, tc.b), tc.want)
	}
}

func TestLanguageVersionTarget(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"1.23", "1.23", true},
		{"go1.23", "1.23", true},
		{"v1.23", "1.23", true},
		{"1.23rc1", "1.23", true}, // pre-release of a bare minor still ambiguous
		{"1.23.0", "", false},     // explicit patch: unambiguous
		{"1.23.7", "", false},
		{"1", "", false}, // no minor
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := languageVersionTarget(tc.in)
			assert.Equal(t, got, tc.want)
			assert.Equal(t, ok, tc.wantOK)
		})
	}
}

func TestSet(t *testing.T) {
	s := set{}
	assert.That(t, !s.has("a"), "empty set should not contain 'a'")
	s.add("b")
	s.add("a")
	s.add("c")
	s.add("a") // dup
	assert.That(t, s.has("a") && s.has("b") && s.has("c"), "missing keys after add")
	assert.Len(t, s, 3)
	assert.EqualArrays(t, s.sorted(), []string{"a", "b", "c"})
}

func TestSnapshotBackupRestore(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\ngo 1.22\n"), 0o644))
	// no go.sum

	snap := backupModFiles()

	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\ngo 1.99\n"), 0o644))
	assert.NoError(t, os.WriteFile("go.sum", []byte("garbage\n"), 0o644))

	snap.restore()

	got, err := os.ReadFile("go.mod")
	assert.NoError(t, err)
	assert.Equal(t, string(got), "module x\ngo 1.22\n")

	_, err = os.Stat("go.sum")
	assert.That(t, os.IsNotExist(err), "go.sum should have been deleted")
}

func TestMinorOf(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"v1.22.3", 22},
		{"v1.0.0", 0},
		{"v2.7.1", 7},
		{"", 0},
		{"v1", 0},
	}
	for _, tc := range cases {
		assert.Equal(t, minorOf(tc.in), tc.want)
	}
}

func TestReadLocalGoDirective(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\n\ngo 1.22\n"), 0o644))
	got, err := readLocalGoDirective()
	assert.NoError(t, err)
	assert.Equal(t, got, "1.22")
}

func TestPatchOf(t *testing.T) {
	cases := []struct {
		in      string
		wantP   int
		wantHas bool
	}{
		{"1.22", 0, false},
		{"1.22.0", 0, true},
		{"1.22.3", 3, true},
		{"go1.22.13", 13, true},
		{"v1.21.99", 99, true},
		{"1.22rc1", 0, false},
		{"1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, has := patchOf(tc.in)
			assert.Equal(t, p, tc.wantP)
			assert.Equal(t, has, tc.wantHas)
		})
	}
}

func TestLatestPatch(t *testing.T) {
	fakeFeed(t, "go1.21", "go1.21.1", "go1.21.13", "go1.21.5", "go1.22", "go1.22rc1", "go1.20")

	p, ok := latestPatch(21)
	assert.That(t, ok, "feed should be reachable")
	assert.Equal(t, p, 13)

	p, ok = latestPatch(22)
	assert.That(t, ok, "feed should be reachable")
	assert.Equal(t, p, 0) // base release only, no patches

	p, ok = latestPatch(99)
	assert.That(t, ok, "feed should be reachable")
	assert.Equal(t, p, -1) // minor not released
}

func TestLatestPatchFeedFailure(t *testing.T) {
	prev, hadPrev := os.LookupEnv("CGV_RELEASE_FEED")
	// 127.0.0.1:1 is reserved/unbound — connection refused, fast.
	os.Setenv("CGV_RELEASE_FEED", "http://127.0.0.1:1/")
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("CGV_RELEASE_FEED", prev)
		} else {
			os.Unsetenv("CGV_RELEASE_FEED")
		}
	})
	patchCache.Lock()
	patchCache.m = map[int]int{}
	patchCache.Unlock()
	t.Cleanup(func() {
		patchCache.Lock()
		patchCache.m = map[int]int{}
		patchCache.Unlock()
	})

	_, ok := latestPatch(21)
	assert.That(t, !ok, "feed failure should report ok=false")
}

func TestValidateTarget(t *testing.T) {
	fakeFeed(t, "go1.21", "go1.21.13", "go1.22", "go1.22.3")

	assert.NoError(t, validateTarget("1.21"))
	assert.NoError(t, validateTarget("1.21.0"))
	assert.NoError(t, validateTarget("1.21.13"))
	assert.NoError(t, validateTarget("1.22.3"))
	assert.NoError(t, validateTarget("go1.21.13"))

	assert.Error(t, validateTarget("1.21.99"), `^go 1\.21\.99 is not a released Go version \(latest patch is 1\.21\.13\)$`)
	assert.Error(t, validateTarget("1.99"), `^go 1\.99 is not a released Go version$`)
	assert.Error(t, validateTarget("1.99.0"), `^go 1\.99 is not a released Go version$`)
}

func TestValidateTargetFeedFailure(t *testing.T) {
	prev, hadPrev := os.LookupEnv("CGV_RELEASE_FEED")
	os.Setenv("CGV_RELEASE_FEED", "http://127.0.0.1:1/")
	t.Cleanup(func() {
		if hadPrev {
			os.Setenv("CGV_RELEASE_FEED", prev)
		} else {
			os.Unsetenv("CGV_RELEASE_FEED")
		}
	})
	patchCache.Lock()
	patchCache.m = map[int]int{}
	patchCache.Unlock()
	t.Cleanup(func() {
		patchCache.Lock()
		patchCache.m = map[int]int{}
		patchCache.Unlock()
	})

	// Feed unreachable: validation must pass through (offline mode).
	assert.NoError(t, validateTarget("1.21.99"))
}

// If go.mod starts with an unreleased / invalid go directive (e.g. 1.21.99),
// the snapshot must preserve and restore those exact bytes. We don't validate
// the *current* directive — only the user-supplied target — so an invalid
// baseline is fine, and aborts must put the file back exactly as found.
func TestSnapshotPreservesInvalidGoDirective(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	original := "module x\n\ngo 1.21.99\n"
	assert.NoError(t, os.WriteFile("go.mod", []byte(original), 0o644))

	snap := backupModFiles()

	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\n\ngo 1.22\n"), 0o644))
	assert.NoError(t, os.WriteFile("go.sum", []byte("scratch\n"), 0o644))

	snap.restore()

	got, err := os.ReadFile("go.mod")
	assert.NoError(t, err)
	assert.Equal(t, string(got), original)

	_, err = os.Stat("go.sum")
	assert.That(t, os.IsNotExist(err), "go.sum should have been deleted on restore")
}

// readLocalGoDirective must return whatever go directive go.mod currently has,
// even if it names an unreleased patch. validateTarget only ever runs against
// the user-supplied target, never against this value.
func TestReadLocalGoDirectiveInvalid(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\n\ngo 1.21.99\n"), 0o644))
	got, err := readLocalGoDirective()
	assert.NoError(t, err)
	assert.Equal(t, got, "1.21.99")
}

func TestCheckLocalGoDirective(t *testing.T) {
	fakeFeed(t, "go1.21", "go1.21.13", "go1.22", "go1.22.3")

	dir := t.TempDir()
	chdir(t, dir)

	// Valid directive: no warning.
	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\n\ngo 1.21.13\n"), 0o644))
	assert.Equal(t, checkLocalGoDirective(), "")

	// Invalid patch: warn but non-empty.
	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\n\ngo 1.21.99\n"), 0o644))
	got := checkLocalGoDirective()
	assert.ContainsString(t, got, "current go.mod")
	assert.ContainsString(t, got, "1.21.99")
	assert.ContainsString(t, got, "proceeding anyway")

	// Invalid minor: also warns.
	assert.NoError(t, os.WriteFile("go.mod", []byte("module x\n\ngo 1.99\n"), 0o644))
	got = checkLocalGoDirective()
	assert.ContainsString(t, got, "1.99")
	assert.ContainsString(t, got, "proceeding anyway")

	// Missing go.mod: silent (caller handles existence).
	assert.NoError(t, os.Remove("go.mod"))
	assert.Equal(t, checkLocalGoDirective(), "")
}

func TestSnapshotRestoreMissing(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	snap := backupModFiles()

	assert.NoError(t, os.WriteFile("go.mod", []byte("x"), 0o644))
	assert.NoError(t, os.WriteFile("go.sum", []byte("y"), 0o644))

	snap.restore()

	for _, p := range []string{"go.mod", "go.sum"} {
		_, err := os.Stat(filepath.Join(dir, p))
		assert.That(t, os.IsNotExist(err), p+" should not exist after restore")
	}
}
