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

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want goVersion
	}{
		{"1.22", goVersion{1, 22, 0}},
		{"1.22.3", goVersion{1, 22, 3}},
		{"1.22.0", goVersion{1, 22, 0}},
		{"go1.22.3", goVersion{1, 22, 3}},
		{"1.22-rc1", goVersion{1, 22, 1}},
		{"v1.22.3", goVersion{1, 22, 3}},
		{"", goVersion{0, 0, 0}},
		{"1", goVersion{1, 0, 0}},
		{"1.22.3.4", goVersion{1, 22, 3}}, // 4th part dropped
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseVersion(tc.in); got != tc.want {
				t.Errorf("parseVersion(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestGoVersionCompare(t *testing.T) {
	cases := []struct {
		a, b goVersion
		want int
	}{
		{goVersion{1, 22, 0}, goVersion{1, 22, 0}, 0},
		{goVersion{1, 21, 0}, goVersion{1, 22, 0}, -1},
		{goVersion{1, 22, 1}, goVersion{1, 22, 0}, 1},
		{goVersion{1, 22, 0}, goVersion{1, 22, 1}, -1},
		{goVersion{2, 0, 0}, goVersion{1, 99, 99}, 1},
		{goVersion{1, 22, 3}, goVersion{1, 22, 3}, 0},
	}
	for _, tc := range cases {
		if got := tc.a.Compare(tc.b); got != tc.want {
			t.Errorf("%v.Compare(%v) = %d, want %d", tc.a, tc.b, got, tc.want)
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
