//go:build integration

package integration

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lczyk/assert"
	"golang.org/x/mod/modfile"
)

// repoRoot is the absolute path of the repository root.
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}()

// proxyURL is the file:// URL Go's GOPROXY accepts.
var proxyURL = "file://" + filepath.ToSlash(filepath.Join(repoRoot, "integration", "proxy"))

// sumsPath is the location of the SUMS file produced by buildproxy.
var sumsPath = filepath.Join(repoRoot, "integration", "proxy", "SUMS")

// binPath is set by TestMain to the path of the freshly built CLI.
var binPath string

// modCache is shared across subtests; populated lazily by Go itself.
var modCache string

// copyFile copies src to dst. Fails the test on error.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	assert.NoError(t, err, "open %s", src)
	defer in.Close()
	assert.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	out, err := os.Create(dst)
	assert.NoError(t, err, "create %s", dst)
	defer out.Close()
	_, err = io.Copy(out, in)
	assert.NoError(t, err, "copy %s -> %s", src, dst)
}

// copyDir copies all regular files from src to dst, preserving relative paths.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		copyFile(t, p, target)
		return nil
	})
	assert.NoError(t, err, "copy dir %s", src)
}

// setupFixture copies fixtures/<name>/ to a t.TempDir, plus SUMS → go.sum.
// Returns the tempdir path.
func setupFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join(repoRoot, "integration", "fixtures", name)
	dst := t.TempDir()
	copyDir(t, src, dst)
	copyFile(t, sumsPath, filepath.Join(dst, "go.sum"))
	return dst
}

type runResult struct {
	stdout, stderr string
	exit           int
}

// runTool invokes the built CLI in dir with the given args. Captures stdio.
func runTool(t *testing.T, dir string, args ...string) runResult {
	return runToolEnv(t, dir, nil, args...)
}

// runToolEnv is runTool with extra "KEY=value" env entries appended.
func runToolEnv(t *testing.T, dir string, extraEnv []string, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	// GOSUMDB=off because our zips strip upstream source down to go.mod +
	// `package` stubs — h1 hashes never match sum.golang.org. Pre-stuffing
	// go.sum from SUMS is fragile across multi-step tests (tidy prunes
	// unused entries between runs), so we drop the sumdb check entirely.
	env := append(os.Environ(),
		"GOPROXY="+proxyURL+",off",
		"GOSUMDB=off",
		"GOFLAGS=-mod=mod",
		"GOMODCACHE="+modCache,
		"GOTOOLCHAIN=local",
	)
	cmd.Env = append(env, extraEnv...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	r := runResult{stdout: so.String(), stderr: se.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.exit = ee.ExitCode()
	} else {
		assert.NoError(t, err, "run %s\nstderr: %s", binPath, r.stderr)
	}
	return r
}

// loadGoMod parses dir/go.mod.
func loadGoMod(t *testing.T, dir string) *modfile.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	assert.NoError(t, err, "read go.mod")
	f, err := modfile.Parse("go.mod", data, nil)
	assert.NoError(t, err, "parse go.mod")
	return f
}

// requireVersion returns the version string for mod from f, or fails.
func requireVersion(t *testing.T, f *modfile.File, mod string) string {
	t.Helper()
	for _, r := range f.Require {
		if r.Mod.Path == mod {
			return r.Mod.Version
		}
	}
	assert.That(t, false, "require %s not found", mod)
	return ""
}

// readBytes returns the byte contents of dir/name or fails.
func readBytes(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	assert.NoError(t, err, "read %s", name)
	return b
}

// bytesEqual asserts a == b with a fail message.
func bytesEqual(t *testing.T, a, b []byte, msg string) {
	t.Helper()
	assert.That(t, bytes.Equal(a, b), "%s: differ\n--- got ---\n%s\n--- want ---\n%s", msg, a, b)
}

// assertOK asserts the tool exited zero; stderr is included on failure.
func assertOK(t *testing.T, r runResult) {
	t.Helper()
	assert.That(t, r.exit == 0, "exit=%d\nstderr:\n%s", r.exit, r.stderr)
}

// assertFailed asserts the tool exited non-zero; stderr is included on failure.
func assertFailed(t *testing.T, r runResult) {
	t.Helper()
	assert.That(t, r.exit != 0, "expected non-zero exit\nstderr:\n%s", r.stderr)
}
