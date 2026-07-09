// change-go-version sets the module's `go` directive to TARGET, then moves every
// dep to the highest version whose own go.mod declares `go <= TARGET`.
// Works both directions (downgrade if TARGET < current, upgrade-within-cap if
// TARGET > current).
//
// Usage: go run github.com/lczyk/change-go-version@latest [flags] [target]
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	goversion "go/version"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	flags "github.com/jessevdk/go-flags"
	version "github.com/lczyk/version/go"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

//go:embed VERSION
var versionFile string

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

// goToken renders a Go directive value ("1.24", "go1.24.3", "v1.24") as the
// "go1.24" form that go/version expects.
func goToken(v string) string {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "go")
	return "go" + v
}

// compareGo orders two Go directive values using the Go toolchain's own
// ordering, where a bare language version "1.23" sorts *below* the release
// "1.23.0" (and below the pre-release "1.23rc1"). A plain semver compare gets
// this wrong by treating "1.23" and "1.23.0" as equal -- but a dependency
// declaring "go 1.23.0" is genuinely not satisfied by a 1.23 toolchain, so the
// distinction is load-bearing when we decide which deps a target admits.
func compareGo(a, b string) int { return goversion.Compare(goToken(a), goToken(b)) }

// languageVersionTarget reports whether target names a Go *language* version
// like "1.23" (major.minor, no patch) rather than a specific release like
// "1.23.0", and returns the cleaned "1.23" form. The two are not
// interchangeable in a go.mod `go` directive: "1.23" sorts below "1.23.0".
func languageVersionTarget(target string) (clean string, ok bool) {
	v := normalizeTarget(target)
	for i, r := range v {
		if !(r >= '0' && r <= '9' || r == '.') {
			v = v[:i]
			break
		}
	}
	v = strings.TrimRight(v, ".")
	parts := strings.Split(v, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return v, true
}

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

// normalizeTarget strips a leading "v" or "go" prefix and returns the form
// that should appear in the go.mod `go` directive (e.g. "1.24" or "1.24.0").
func normalizeTarget(target string) string {
	v := strings.TrimPrefix(target, "v")
	v = strings.TrimPrefix(v, "go")
	return v
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
	goVer := normalizeTarget(target)
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

// minorOf returns the second component (X) of a canonical "vMAJOR.MINOR.PATCH" Go version.
func minorOf(canonical string) int {
	s := strings.TrimPrefix(canonical, "v")
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}

// readLocalGoDirective returns the `go` directive value from ./go.mod (e.g. "1.22").
func readLocalGoDirective() (string, error) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return "", err
	}
	f, err := modfile.ParseLax("go.mod", data, nil)
	if err != nil {
		return "", err
	}
	if f.Go == nil {
		return "", fmt.Errorf("go.mod has no go directive")
	}
	return f.Go.Version, nil
}

// runCheck runs the user's verification command via /bin/sh -c, streaming stdio.
func runCheck(cmd string) error {
	c := exec.Command("sh", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// patchOf returns the third component (Y) of a canonical "vMAJOR.MINOR.PATCH"
// Go version, and whether the original target string had an explicit patch.
func patchOf(target string) (patch int, hasPatch bool) {
	v := normalizeTarget(target)
	for i, r := range v {
		if !(r >= '0' && r <= '9' || r == '.') {
			v = v[:i]
			break
		}
	}
	v = strings.TrimRight(v, ".")
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, false
	}
	return n, true
}

// validateTarget rejects targets whose minor or patch is not a released Go
// version. Feed-fetch failures pass through with a warning so offline use
// still works.
func validateTarget(target string) error {
	minor := minorOf(canonGoVersion(target))
	maxP, ok := latestPatch(minor)
	if !ok {
		return nil
	}
	if maxP < 0 {
		return fmt.Errorf("go 1.%d is not a released Go version", minor)
	}
	patch, hasPatch := patchOf(target)
	if hasPatch && patch > maxP {
		return fmt.Errorf("go 1.%d.%d is not a released Go version (latest patch is 1.%d.%d)", minor, patch, minor, maxP)
	}
	return nil
}

// checkLocalGoDirective returns a warning message if go.mod's current go
// directive names an unreleased version (or empty if it looks valid /
// unreadable / the feed is unreachable). The directive is informational only
// — we never refuse to run because of it.
func checkLocalGoDirective() string {
	cur, err := readLocalGoDirective()
	if err != nil {
		return ""
	}
	if e := validateTarget(cur); e != nil {
		return fmt.Sprintf("current go.mod %v; proceeding anyway", e)
	}
	return ""
}

func runChange(target string, rounds, jobs int, noTidy bool) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	goVer := normalizeTarget(target)

	if err := editLocalGoMod(target); err != nil {
		return err
	}

	info("Pinning direct deps to highest version with go <= %s", goVer)
	direct := make([]string, 0)
	for _, r := range listModules(true) {
		direct = append(direct, r.Path)
	}
	if bad := pinBatch(direct, target, "", jobs); len(bad) > 0 {
		return fmt.Errorf("no version compatible with go %s for direct dep(s): %s",
			goVer, strings.Join(bad.sorted(), ", "))
	}
	// `go get` may raise the go directive when a fetched module declares
	// `go > current`. Re-pin to the user's target so the bump never sticks.
	if err := editLocalGoMod(target); err != nil {
		return err
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
				if err := runStream("go", "mod", "tidy", "-go="+goVer); err != nil {
					return fmt.Errorf("go mod tidy: %w", err)
				}
			}
			info("Done. go directive: %s", goVer)
			return nil
		}
		info("Round %d: %d offending indirect(s)", n, len(offenders))
		for m := range pinBatch(offenders, target, fmt.Sprintf("[round %d] ", n), jobs) {
			skip.add(m)
		}
		if err := editLocalGoMod(target); err != nil {
			return err
		}
	}
	return fmt.Errorf("hit max rounds (%d); indirects still violate target", rounds)
}

// tryVersion restores baseline, applies cand via runChange, then runs checkCmd.
// Returns true iff both steps succeed.
func tryVersion(cand string, rounds, jobs int, noTidy bool, baseline snapshot, checkCmd string) bool {
	baseline.restore()
	info("Auto: trying go %s ...", cand)
	if err := runChange(cand, rounds, jobs, noTidy); err != nil {
		warn("Auto: change to %s failed: %v", cand, err)
		return false
	}
	if err := runCheck(checkCmd); err != nil {
		warn("Auto: check failed at go %s", cand)
		return false
	}
	info("Auto: go %s passed", cand)
	return true
}

// patchCache memoizes latestPatch results per minor.
var patchCache = struct {
	sync.Mutex
	m map[int]int
}{m: map[int]int{}}

// latestPatch returns the highest released patch Y for "go1.<minor>.Y" from
// the official Go release feed. Returns (0, true) if only "go1.<minor>" (no
// patches) exists, (-1, true) if the minor itself is not released, or
// (-1, false) on fetch/parse failure.
func latestPatch(minor int) (int, bool) {
	patchCache.Lock()
	defer patchCache.Unlock()
	if v, ok := patchCache.m[minor]; ok {
		return v, true
	}
	client := &http.Client{Timeout: 10 * time.Second}
	feedURL := "https://go.dev/dl/?mode=json&include=all"
	if v := os.Getenv("CGV_RELEASE_FEED"); v != "" {
		feedURL = v
	}
	resp, err := client.Get(feedURL)
	if err != nil {
		warn("fetch go release list: %v", err)
		return -1, false
	}
	defer resp.Body.Close()
	var rels []struct{ Version string }
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		warn("parse go release list: %v", err)
		return -1, false
	}
	prefix := fmt.Sprintf("go1.%d", minor)
	best := -1
	for _, r := range rels {
		if !strings.HasPrefix(r.Version, prefix) {
			continue
		}
		rest := r.Version[len(prefix):]
		if rest == "" {
			if best < 0 {
				best = 0
			}
			continue
		}
		if rest[0] != '.' {
			continue // skip rc/beta of base 1.X
		}
		p, err := strconv.Atoi(rest[1:])
		if err != nil {
			continue
		}
		if p > best {
			best = p
		}
	}
	patchCache.m[minor] = best
	return best, true
}

// runAuto walks the go directive downwards by minor (1.X), accepting each
// passing version. On a failing minor, it tries the latest released patch
// for that minor (1.X.<latest>); if that fails the search ends (no lower
// patch can plausibly help). If it passes, patches are walked downward from
// latest-1; each passing one becomes the new accepted version, and the walk
// stops at the first failing patch. Final result is the lowest passing
// version found (or baseline if none).
func runAuto(checkCmd string, rounds, jobs int, noTidy bool, baseline snapshot) error {
	initial, err := readLocalGoDirective()
	if err != nil {
		return err
	}
	info("Auto: baseline go %s; verifying check command...", initial)
	if err := runCheck(checkCmd); err != nil {
		return fmt.Errorf("baseline check failed at go %s: %w", initial, err)
	}

	startMinor := minorOf(canonGoVersion(initial))
	lastGood := initial
	lastGoodSnap := baseline
outer:
	for x := startMinor - 1; x >= 0; x-- {
		cand := fmt.Sprintf("1.%d", x)
		if tryVersion(cand, rounds, jobs, noTidy, baseline, checkCmd) {
			lastGood = cand
			lastGoodSnap = backupModFiles()
			continue
		}
		maxP, _ := latestPatch(x)
		if maxP <= 0 {
			info("Auto: 1.%d.0 failed; no released patches to try", x)
			break
		}
		topCand := fmt.Sprintf("1.%d.%d", x, maxP)
		info("Auto: 1.%d.0 failed; trying latest patch %s", x, topCand)
		if !tryVersion(topCand, rounds, jobs, noTidy, baseline, checkCmd) {
			break outer
		}
		lastGood = topCand
		lastGoodSnap = backupModFiles()
		for y := maxP - 1; y >= 1; y-- {
			cand := fmt.Sprintf("1.%d.%d", x, y)
			if !tryVersion(cand, rounds, jobs, noTidy, baseline, checkCmd) {
				break outer
			}
			lastGood = cand
			lastGoodSnap = backupModFiles()
		}
		break outer
	}

	lastGoodSnap.restore()
	if canonGoVersion(lastGood) == canonGoVersion(initial) {
		info("Auto: no version below %s passes; baseline restored", initial)
	} else {
		info("Auto: lowest passing go version: %s (applied)", lastGood)
	}
	return nil
}

type options struct {
	To      string `long:"to" description:"target Go version, e.g. 1.22 (mutually exclusive with --auto)"`
	Auto    string `long:"auto" description:"verification command run via /bin/sh -c; finds lowest passing version (mutually exclusive with --to)"`
	Dir     string `short:"d" long:"dir" default:"." description:"module directory containing go.mod"`
	Rounds  int    `long:"rounds" default:"5" description:"max indirect-fixup rounds"`
	Jobs    int    `short:"j" long:"jobs" default:"8" description:"parallel version probes"`
	NoTidy  bool   `long:"no-tidy" description:"skip the final go mod tidy"`
	Version bool   `long:"version" description:"print version and exit"`
}

func main() {
	var opts options
	parser := flags.NewParser(&opts, flags.Default)
	parser.Name = "change-go-version"
	parser.LongDescription = "Move a Go module's go directive (and deps) to a target version, or find the lowest passing version automatically."
	if _, err := parser.Parse(); err != nil {
		if fe, ok := err.(*flags.Error); ok && fe.Type == flags.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1)
	}

	if opts.Version {
		fmt.Println("change-go-version", version.Read(strings.TrimSpace(versionFile)))
		return
	}

	if (opts.To == "") == (opts.Auto == "") {
		errlog("exactly one of --to or --auto is required")
		os.Exit(2)
	}

	if err := os.Chdir(opts.Dir); err != nil {
		errlog("chdir %s: %v", opts.Dir, err)
		os.Exit(1)
	}
	if _, err := os.Stat("go.mod"); err != nil {
		errlog("no go.mod in %s", opts.Dir)
		os.Exit(1)
	}

	if msg := checkLocalGoDirective(); msg != "" {
		warn("%s", msg)
	}

	if v, ok := languageVersionTarget(opts.To); ok {
		warn("go %s is not equivalent to %s.0 -- %q is the Go language version and sorts below the %q release, so deps declaring %s.0 are excluded. See https://go.dev/doc/toolchain#version", v, v, v, v+".0", v)
	}

	snap := backupModFiles()

	// Restore baseline on SIGINT/SIGTERM so an interrupted run leaves go.mod
	// and go.sum exactly as we found them. Without this, killing a long
	// `--auto` mid-iteration leaves the working tree at whatever candidate
	// was being probed.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		errlog("Interrupted (%s); restoring go.mod and go.sum", sig)
		snap.restore()
		os.Exit(130)
	}()

	var runErr error
	if opts.To != "" {
		runErr = runChange(opts.To, opts.Rounds, opts.Jobs, opts.NoTidy)
	} else {
		runErr = runAuto(opts.Auto, opts.Rounds, opts.Jobs, opts.NoTidy, snap)
	}
	if runErr != nil {
		errlog("%v", runErr)
		errlog("Restoring go.mod and go.sum to original state")
		snap.restore()
		os.Exit(1)
	}
}
