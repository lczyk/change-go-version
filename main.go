// change-go-version sets the module's `go` directive to TARGET, then moves every
// dep to the highest version whose own go.mod declares `go <= TARGET`.
// Works both directions (downgrade if TARGET < current, upgrade-within-cap if
// TARGET > current).
//
// Usage: go run github.com/lczyk/change-go-version@latest [flags] [target]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
	colReset  = "\033[0m"
)

func info(format string, a ...any)  { fmt.Fprintf(os.Stderr, colGreen+"INFO"+colReset+": "+format+"\n", a...) }
func warn(format string, a ...any)  { fmt.Fprintf(os.Stderr, colYellow+"WARNING"+colReset+": "+format+"\n", a...) }
func errlog(format string, a ...any) { fmt.Fprintf(os.Stderr, colRed+"ERROR"+colReset+": "+format+"\n", a...) }

type goVersion [3]int

var goLineRe = regexp.MustCompile(`^go\s+(\S+)`)

func norm(v string) goVersion {
	parts := regexp.MustCompile(`[.\-]`).Split(v, -1)
	var out goVersion
	for i := 0; i < 3 && i < len(parts); i++ {
		m := regexp.MustCompile(`\d+`).FindString(parts[i])
		if m == "" {
			continue
		}
		n, _ := strconv.Atoi(m)
		out[i] = n
	}
	return out
}

func cmpVersion(a, b goVersion) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// run executes a go command with GOTOOLCHAIN=local. Captured by default.
func run(capture bool, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if capture {
		var so, se strings.Builder
		cmd.Stdout = &so
		cmd.Stderr = &se
		err = cmd.Run()
		return so.String(), se.String(), err
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	return "", "", err
}

func mustRun(args ...string) {
	if _, stderr, err := run(true, args...); err != nil {
		panic(fmt.Errorf("%s failed: %w\n%s", strings.Join(args, " "), err, stderr))
	}
}

// declaredGo returns the `go` directive from <mod>@<ver>'s go.mod, or "" on failure.
func declaredGo(mod, ver string) string {
	stdout, _, err := run(true, "go", "mod", "download", "-json", mod+"@"+ver)
	if err != nil || stdout == "" {
		return ""
	}
	var info struct {
		GoMod string
	}
	if json.Unmarshal([]byte(stdout), &info) != nil || info.GoMod == "" {
		return ""
	}
	f, err := os.Open(info.GoMod)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := goLineRe.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return "1.0"
}

// listVersions returns versions newest-first.
func listVersions(mod string) []string {
	stdout, _, err := run(true, "go", "list", "-m", "-versions", mod)
	if err != nil {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) <= 1 {
		return nil
	}
	vs := fields[1:]
	for i, j := 0, len(vs)-1; i < j; i, j = i+1, j-1 {
		vs[i], vs[j] = vs[j], vs[i]
	}
	return vs
}

func pickVersion(mod string, target goVersion) (ver, gv string, ok bool) {
	for _, v := range listVersions(mod) {
		gv := declaredGo(mod, v)
		if gv == "" {
			continue
		}
		if cmpVersion(norm(gv), target) <= 0 {
			return v, gv, true
		}
	}
	return "", "", false
}

type modRow struct{ Path, Version, GoVersion string }

func listModules(directOnly bool) []modRow {
	const fmtTpl = "{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.GoVersion}}\t{{.Indirect}}{{end}}"
	stdout, _, _ := run(true, "go", "list", "-mod=mod", "-e", "-m", "-f", fmtTpl, "all")
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
		rows = append(rows, modRow{parts[0], parts[1], parts[2]})
	}
	return rows
}

// pinBatch probes versions for each mod in parallel, then `go get`s them serially.
// Returns the set of mods for which no compatible version exists.
func pinBatch(mods []string, target goVersion, label string, jobs int) map[string]bool {
	type result struct {
		mod, ver, gv string
		ok           bool
	}
	results := make([]result, len(mods))
	sem := make(chan struct{}, max(1, min(jobs, len(mods))))
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

	unresolvable := map[string]bool{}
	for _, r := range results {
		if !r.ok {
			warn("no compatible version for %s", r.mod)
			unresolvable[r.mod] = true
			continue
		}
		info("%s%s -> %s  (declares go %s)", label, r.mod, r.ver, r.gv)
		if _, stderr, err := run(true, "go", "get", r.mod+"@"+r.ver); err != nil {
			lines := strings.Split(strings.TrimSpace(stderr), "\n")
			warn("go get failed for %s: %s", r.mod, lines[len(lines)-1])
		}
	}
	return unresolvable
}

type snapshot map[string][]byte

func backupModFiles() snapshot {
	s := snapshot{}
	for _, p := range []string{"go.mod", "go.sum"} {
		if data, err := os.ReadFile(p); err == nil {
			s[p] = data
		} else {
			s[p] = nil // marker: didn't exist
		}
	}
	return s
}

func restoreModFiles(s snapshot) {
	for p, data := range s {
		if data == nil {
			os.Remove(p)
		} else {
			_ = os.WriteFile(p, data, 0o644)
		}
	}
}

func runMain(target string, rounds, jobs int, noTidy bool) error {
	tgt := norm(target)
	canonical := fmt.Sprintf("%d.%d.%d", tgt[0], tgt[1], tgt[2])

	mustRun("go", "mod", "edit", "-go="+target, "-toolchain=none")

	info("Pinning direct deps to highest version with go <= %s", canonical)
	var direct []string
	for _, r := range listModules(true) {
		direct = append(direct, r.Path)
	}
	if unresolvable := pinBatch(direct, tgt, "", jobs); len(unresolvable) > 0 {
		names := make([]string, 0, len(unresolvable))
		for m := range unresolvable {
			names = append(names, m)
		}
		sort.Strings(names)
		return fmt.Errorf("no version compatible with go %s for direct dep(s): %s", canonical, strings.Join(names, ", "))
	}

	skip := map[string]bool{}
	converged := false
	for n := 1; n <= rounds; n++ {
		var offenders []string
		for _, r := range listModules(false) {
			if r.GoVersion != "" && cmpVersion(norm(r.GoVersion), tgt) > 0 && !skip[r.Path] {
				offenders = append(offenders, r.Path)
			}
		}
		if len(offenders) == 0 {
			converged = true
			break
		}
		info("Round %d: %d offending indirect(s)", n, len(offenders))
		for m := range pinBatch(offenders, tgt, fmt.Sprintf("[round %d] ", n), jobs) {
			skip[m] = true
		}
	}
	if !converged {
		return fmt.Errorf("hit max rounds (%d); indirects still violate target", rounds)
	}

	if !noTidy {
		if _, _, err := run(false, "go", "mod", "tidy", "-go="+canonical); err != nil {
			return fmt.Errorf("go mod tidy: %w", err)
		}
	}
	info("Done. go directive: %s", canonical)
	return nil
}

func main() {
	rounds := flag.Int("rounds", 5, "Max indirect-fixup rounds")
	jobs := flag.Int("j", 8, "Parallel version probes")
	noTidy := flag.Bool("no-tidy", false, "Skip the final `go mod tidy`")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: change-go-version [flags] [target]\n\nSee https://github.com/lczyk/change-go-version")
		flag.PrintDefaults()
	}
	flag.Parse()

	target := "1.24"
	if flag.NArg() > 0 {
		target = flag.Arg(0)
	}

	snap := backupModFiles()
	defer func() {
		if r := recover(); r != nil {
			errlog("%v", r)
			errlog("Restoring go.mod and go.sum to original state")
			restoreModFiles(snap)
			os.Exit(1)
		}
	}()

	if err := runMain(target, *rounds, *jobs, *noTidy); err != nil {
		errlog("%v", err)
		errlog("Restoring go.mod and go.sum to original state")
		restoreModFiles(snap)
		os.Exit(1)
	}
}

