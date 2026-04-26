package main

import (
	"os"
	"path/filepath"
	"testing"
)

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
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
		{"1.22rc1", "v1.22.0"},   // pre-release stripped
		{"1.22-rc1", "v1.22.0"},  // pre-release stripped
		{"v1.22.3", "v1.22.3"},
		{"", "v0.0.0"},
		{"1", "v1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := canonGoVersion(tc.in); got != tc.want {
				t.Errorf("canonGoVersion(%q) = %q, want %q", tc.in, got, tc.want)
			}
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
		{"1.22rc1", "1.22", 0}, // rc dropped, equals release
	}
	for _, tc := range cases {
		if got := compareGo(tc.a, tc.b); got != tc.want {
			t.Errorf("compareGo(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSet(t *testing.T) {
	s := set{}
	if s.has("a") {
		t.Fatal("empty set should not contain 'a'")
	}
	s.add("b")
	s.add("a")
	s.add("c")
	s.add("a") // dup
	if !s.has("a") || !s.has("b") || !s.has("c") {
		t.Fatal("missing keys after add")
	}
	if len(s) != 3 {
		t.Errorf("len = %d, want 3", len(s))
	}
	got := s.sorted()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("sorted len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sorted[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSnapshotBackupRestore(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.WriteFile("go.mod", []byte("module x\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// no go.sum

	snap := backupModFiles()

	// Mutate / create after snapshot.
	if err := os.WriteFile("go.mod", []byte("module x\ngo 1.99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("go.sum", []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap.restore()

	got, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "module x\ngo 1.22\n" {
		t.Errorf("go.mod not restored: %q", got)
	}
	if _, err := os.Stat("go.sum"); !os.IsNotExist(err) {
		t.Errorf("go.sum should have been deleted, stat err: %v", err)
	}
}

func TestSnapshotRestoreMissing(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// neither file exists
	snap := backupModFiles()

	// Create both, then restore should remove them.
	if err := os.WriteFile("go.mod", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("go.sum", []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap.restore()

	for _, p := range []string{"go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s should not exist after restore, err: %v", p, err)
		}
	}
}
