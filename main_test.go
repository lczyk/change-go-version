package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"
)

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
		{"1.22", "1.22.0", 0},
		{"1.21", "1.22", -1},
		{"1.22.1", "1.22", 1},
		{"1.22", "1.22.1", -1},
		{"2.0", "1.99.99", 1},
		{"1.22.3", "1.22.3", 0},
		{"go1.22", "1.22.0", 0},
		{"1.22rc1", "1.22", 0},
	}
	for _, tc := range cases {
		assert.Equal(t, compareGo(tc.a, tc.b), tc.want)
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
