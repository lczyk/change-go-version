// change-go-version sets the module's `go` directive to TARGET, then moves every
// dep to the highest version whose own go.mod declares `go <= TARGET`.
// Works both directions (downgrade if TARGET < current, upgrade-within-cap if
// TARGET > current).
//
// Usage: go run github.com/lczyk/change-go-version@latest [flags] [target]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
	colReset  = "\033[0m"
)

func info(format string, a ...any) {
	fmt.Fprintf(os.Stderr, colGreen+"INFO"+colReset+": "+format+"\n", a...)
}
func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, colYellow+"WARNING"+colReset+": "+format+"\n", a...)
}
func errlog(format string, a ...any) {
	fmt.Fprintf(os.Stderr, colRed+"ERROR"+colReset+": "+format+"\n", a...)
}

// canonGoVersion returns a semver-canonical "vMAJOR.MINOR.PATCH" form for
// a Go directive value like "1.24", "1.24.3", "go1.24.3", or "1.24rc1".
// Pre-release suffixes (rc1, beta1, etc.) are dropped — we treat them as
// the underlying release for compatibility comparisons.
func canonGoVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "go")
	for i, r := range v {
		if !(r >= '0' && r <= '9' || r == '.') {
			v = v[:i]
			break
		}
	}
	v = strings.TrimRight(v, ".")
	if v == "" {
		return "v0.0.0"
	}
	return semver.Canonical("v" + v)
}

// compareGo orders two Go directive values; returns -1, 0, or 1.
func compareGo(a, b string) int { return semver.Compare(canonGoVersion(a), canonGoVersion(b)) }

func goEnv() []string { return append(os.Environ(), "GOTOOLCHAIN=local") }

// runCapture runs a command and captures stdout/stderr.
func runCapture(name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(name, args...)
	cmd.Env = goEnv()
	var so, se strings.Builder
	cmd.Stdout = &so
	cmd.Stderr = &se
	err = cmd.Run()
	return so.String(), se.String(), err
}

// runStream runs a command with stdout/stderr passed through.
func runStream(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = goEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// editLocalGoMod sets the `go` directive to target and drops `toolchain` from
// ./go.mod, parsing and rewriting the file directly (no `go mod edit`
// subprocess). target is the user-supplied form, e.g. "1.24" or "1.24.0".
func editLocalGoMod(target string) error {
	const path = "go.mod"
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	goVer := strings.TrimPrefix(target, "v")
	goVer = strings.TrimPrefix(goVer, "go")
	if err := f.AddGoStmt(goVer); err != nil {
		return fmt.Errorf("add go directive %q: %w", goVer, err)
	}
	f.DropToolchainStmt()
	out, err := f.Format()
	if err != nil {
		return fmt.Errorf("format %s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
}

// declaredGo returns the `go` directive from <mod>@<ver>'s go.mod, or "" on failure.
func declaredGo(mod, ver string) string {
	mv := module.Version{Path: mod, Version: ver}
	stdout, _, err := runCapture("go", "mod", "download", "-json", mv.String())
	if err != nil || stdout == "" {
		return ""
	}
	var meta struct{ GoMod string }
	if err := json.Unmarshal([]byte(stdout), &meta); err != nil || meta.GoMod == "" {
		return ""
	}
	data, err := os.ReadFile(meta.GoMod)
	if err != nil {
		return ""
	}
	f, err := modfile.ParseLax(meta.GoMod, data, nil)
	if err != nil || f.Go == nil {
		return "1.0"
	}
	return f.Go.Version
}

// listVersions returns versions newest-first.
func listVersions(mod string) []string {
	stdout, _, err := runCapture("go", "list", "-m", "-versions", mod)
	if err != nil {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) <= 1 {
		return nil
	}
	vs := fields[1:]
	slices.Reverse(vs)
	return vs
}

func pickVersion(mod, target string) (ver, declared string, ok bool) {
	for _, v := range listVersions(mod) {
		gv := declaredGo(mod, v)
		if gv == "" {
			continue
		}
		if compareGo(gv, target) <= 0 {
			return v, gv, true
		}
	}
	return "", "", false
}

type modRow struct{ Path, Version, GoVersion string }

func listModules(directOnly bool) []modRow {
	const fmtTpl = "{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.GoVersion}}\t{{.Indirect}}{{end}}"
	stdout, _, _ := runCapture("go", "list", "-mod=mod", "-e", "-m", "-f", fmtTpl, "all")
	var rows []modRow
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		for len(parts) < 4 {
			parts = append(parts, "")
		}
		if directOnly && parts[3] == "true" {
			continue
		}
		if err := module.CheckPath(parts[0]); err != nil {
			continue
		}
		rows = append(rows, modRow{parts[0], parts[1], parts[2]})
	}
	return rows
}

type set map[string]struct{}

func (s set) add(k string)      { s[k] = struct{}{} }
func (s set) has(k string) bool { _, ok := s[k]; return ok }
func (s set) sorted() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// pinBatch probes versions for each mod in parallel, then `go get`s them serially.
// Returns mods for which no compatible version exists.
func pinBatch(mods []string, target, label string, jobs int) set {
	type result struct {
		mod, ver, gv string
		ok           bool
	}
	results := make([]result, len(mods))
	limit := max(1, min(jobs, len(mods)))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, m := range mods {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, m string) {
			defer wg.Done()
			defer func() { <-sem }()
			v, gv, ok := pickVersion(m, target)
			results[i] = result{m, v, gv, ok}
		}(i, m)
	}
	wg.Wait()

	unresolvable := set{}
	for _, r := range results {
		if !r.ok {
			warn("no compatible version for %s", r.mod)
			unresolvable.add(r.mod)
			continue
		}
		info("%s%s -> %s  (declares go %s)", label, r.mod, r.ver, r.gv)
		mv := module.Version{Path: r.mod, Version: r.ver}
		if _, stderr, err := runCapture("go", "get", mv.String()); err != nil {
			lines := strings.Split(strings.TrimSpace(stderr), "\n")
			warn("go get failed for %s: %s", r.mod, lines[len(lines)-1])
		}
	}
	return unresolvable
}

type snapshot struct {
	files map[string][]byte // nil value = file did not exist
}

func backupModFiles() snapshot {
	s := snapshot{files: map[string][]byte{}}
	for _, p := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(p)
		if err != nil {
			s.files[p] = nil
			continue
		}
		s.files[p] = data
	}
	return s
}

func (s snapshot) restore() {
	for p, data := range s.files {
		if data == nil {
			os.Remove(p)
			continue
		}
		_ = os.WriteFile(p, data, 0o644)
	}
}

func run(target string, rounds, jobs int, noTidy bool) error {
	canonical := strings.TrimPrefix(canonGoVersion(target), "v")

	if err := editLocalGoMod(target); err != nil {
		return err
	}

	info("Pinning direct deps to highest version with go <= %s", canonical)
	direct := make([]string, 0)
	for _, r := range listModules(true) {
		direct = append(direct, r.Path)
	}
	if bad := pinBatch(direct, target, "", jobs); len(bad) > 0 {
		return fmt.Errorf("no version compatible with go %s for direct dep(s): %s",
			canonical, strings.Join(bad.sorted(), ", "))
	}

	skip := set{}
	for n := 1; n <= rounds; n++ {
		var offenders []string
		for _, r := range listModules(false) {
			if r.GoVersion != "" && compareGo(r.GoVersion, target) > 0 && !skip.has(r.Path) {
				offenders = append(offenders, r.Path)
			}
		}
		if len(offenders) == 0 {
			if !noTidy {
				if err := runStream("go", "mod", "tidy", "-go="+canonical); err != nil {
					return fmt.Errorf("go mod tidy: %w", err)
				}
			}
			info("Done. go directive: %s", canonical)
			return nil
		}
		info("Round %d: %d offending indirect(s)", n, len(offenders))
		for m := range pinBatch(offenders, target, fmt.Sprintf("[round %d] ", n), jobs) {
			skip.add(m)
		}
	}
	return fmt.Errorf("hit max rounds (%d); indirects still violate target", rounds)
}

func main() {
	rounds := flag.Int("rounds", 5, "Max indirect-fixup rounds")
	jobs := flag.Int("j", 8, "Parallel version probes")
	noTidy := flag.Bool("no-tidy", false, "Skip the final `go mod tidy`")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: change-go-version [flags] <dir> [target]\n\nSee https://github.com/lczyk/change-go-version")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)
	target := "1.24"
	if flag.NArg() > 1 {
		target = flag.Arg(1)
	}

	if err := os.Chdir(dir); err != nil {
		errlog("chdir %s: %v", dir, err)
		os.Exit(1)
	}
	if _, err := os.Stat("go.mod"); err != nil {
		errlog("no go.mod in %s", dir)
		os.Exit(1)
	}

	snap := backupModFiles()
	if err := run(target, *rounds, *jobs, *noTidy); err != nil {
		errlog("%v", err)
		errlog("Restoring go.mod and go.sum to original state")
		snap.restore()
		os.Exit(1)
	}
}
