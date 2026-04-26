# change-go-version

![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/lczyk/change-go-version)
![GitHub Tag](https://img.shields.io/github/v/tag/lczyk/change-go-version?label=release)
[![lint_and_test](https://github.com/lczyk/change-go-version/actions/workflows/lint_and_test.yml/badge.svg)](https://github.com/lczyk/change-go-version/actions/workflows/lint_and_test.yml)

set `go` directive to a target version, then move every dependency to the highest version whose own `go.mod` declares `go <= TARGET`.

this works in both directions -- downgrade your `go` directive and the script walks deps backwards to compatible versions; raise it and the script bumps deps forward up to the new ceiling.

**NOTE**: we we not run the testas after the version change. we *only* change the versions. you then need to run the tests and deal with the fallout yourself.

---

this tool fills a gap in the Go toolchain: `go mod tidy -go=X` errors out when any selected dep needs a newer Go than X, instead of cascading the downgrade. `go get -u ./...` upgrades everything to `@latest` and silently raises your `go` directive to whatever those need. neither does what you usually want when pinning to a specific Go version.

## why

why might you want to downgrade your go version? just read https://blog.howardjohn.info/posts/go-mod-version/. tldr: go version if viral. if you are writing a library and set go version to 1.23, your users *CANNOT* use that lib with earlier go version. is that a real or an arbitrary barrier?

## usage

one of `--to <version>` or `--auto "<cmd>"` is required (mutually exclusive).

```sh
# pin go directive to 1.24 in current dir
go run github.com/lczyk/change-go-version@latest --to 1.24

# pin in some other dir
go run github.com/lczyk/change-go-version@latest --to 1.24 --dir path/to/module

# walk down from current go directive; apply lowest version where tests pass
go run github.com/lczyk/change-go-version@latest --auto "go test ./..."
```

## flags

```
  --to <version>    Target Go version, e.g. 1.22
  --auto <cmd>      Verification command run via /bin/sh -c
  -d, --dir         Module directory containing go.mod (default: .)
  --rounds          Max indirect-fixup rounds (default: 5)
  -j, --jobs        Parallel version probes (default: 8)
  --no-tidy         Skip the final `go mod tidy`
```

## auto mode

reads the current `go` directive, verifies `--check` passes at the baseline, then for each minor version below current (e.g. 1.24 → 1.23 → 1.22 …) it: restores the original `go.mod`/`go.sum`, runs `change` to that candidate, then runs `--check`. it stops on the first failure. the lowest passing version is left applied; if nothing below the baseline passes, baseline is restored unchanged.

## behaviour

1. run `go mod edit -go=TARGET -toolchain=none`
2. **Pass 1:** for every direct dep, list available versions newest→oldest, find the first whose own `go.mod` declares `go <= TARGET`, pin it via `go get`
3. **Pass 2 (rounds):** scan all modules (incl. indirect) for any whose
   currently-selected version still declares `go > TARGET`. pin each down.
   repeat until stable or `-rounds` exhausted
4. run `go mod tidy -go=TARGET`

`GOTOOLCHAIN=local` is set throughout to prevent the Go toolchain from
auto-bumping the directive behind your back.

## failures

we snapshot `go.mod` and `go.sum` at startup. if anything fails, e.g. 
unresolvable direct dep, max rounds exhausted, `go mod tidy` errors,
`Ctrl-C` -- both files are restored before exit.

a common failure is that a direct dep has **no** version compatible with your TARGET (e.g. its earliest release already requires a newer Go). The script reports which deps and exits non-zero with `go.mod` untouched. You then either pick a higher TARGET, or fork/replace the offending dep.

we do **NOT** run go tests after we change the version so you will need to still check all works after the version change.

## Requirements

- Go 1.21+ (uses `min`/`max` builtins).
- Network access to the module proxy (probing dep `go.mod` files).

## Why not just X?

- `go mod tidy -go=X` -- errors on conflicts, doesn't cascade-downgrade
- `go get -u ./...` -- upgrades everything to latest, can raise the `go`
  directive past your target.
- [`marwan-at-work/mod`](https://github.com/marwan-at-work/mod) -- focus on upgrades, not pinning to a Go version
- [`oligot/go-mod-upgrade`](https://github.com/oligot/go-mod-upgrade) -- also focused on upgrades

## License

MIT
