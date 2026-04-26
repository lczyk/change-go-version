# change-go-version

Set a Go module's `go` directive to a target version, then move every dependency
to the highest version whose own `go.mod` declares `go <= TARGET`.

Works in **both directions** — downgrade your `go` directive and the script
walks deps backwards to compatible versions; raise it and the script bumps deps
forward up to the new ceiling.

This fills a gap in the Go toolchain: `go mod tidy -go=X` errors out when any
selected dep needs a newer Go than X, instead of cascading the downgrade.
`go get -u ./...` upgrades everything to `@latest` and silently raises your `go`
directive to whatever those need. Neither does what you usually want when
pinning to a specific Go version.

## Install / Use

No install — run directly:

```sh
go run github.com/lczyk/change-go-version@latest 1.24
```

Or pin a version:

```sh
go run github.com/lczyk/change-go-version@v0.1.0 1.24
```

Run from your module's root (where `go.mod` lives).

## Flags

```
change-go-version [flags] [target]

  target            Target Go version (default: 1.24). E.g. 1.21, 1.24, 1.25.
  -rounds int       Max indirect-fixup rounds (default: 5)
  -j int            Parallel version probes (default: 8)
  -no-tidy          Skip the final `go mod tidy`
```

## Examples

Downgrade to 1.23:

```sh
go run github.com/lczyk/change-go-version@latest 1.23
```

Upgrade to 1.25 with 16 parallel probes:

```sh
go run github.com/lczyk/change-go-version@latest -j 16 1.25
```

## Behaviour

1. Run `go mod edit -go=TARGET -toolchain=none`.
2. **Pass 1:** for every direct dep, list available versions newest→oldest, find
   the first whose own `go.mod` declares `go <= TARGET`, pin it via `go get`.
3. **Pass 2 (rounds):** scan all modules (incl. indirect) for any whose
   currently-selected version still declares `go > TARGET`. Pin each down.
   Repeat until stable or `-rounds` exhausted.
4. Run `go mod tidy -go=TARGET`.

`GOTOOLCHAIN=local` is set throughout to prevent the Go toolchain from
auto-bumping the directive behind your back.

## Failure handling

The script snapshots `go.mod` and `go.sum` at startup. If anything fails —
unresolvable direct dep, max rounds exhausted, `go mod tidy` errors,
`Ctrl-C` — both files are restored byte-for-byte before exit.

Common failure: a direct dep has **no** version compatible with your TARGET
(e.g. its earliest release already requires a newer Go). The script reports
which deps and exits non-zero with `go.mod` untouched. You then either pick
a higher TARGET, or fork/replace the offending dep.

## Requirements

- Go 1.21+ (uses `min`/`max` builtins).
- Network access to the module proxy (probing dep `go.mod` files).

## Why not just X?

- `go mod tidy -go=X` — errors on conflicts, doesn't cascade-downgrade.
- `go get -u ./...` — upgrades everything to latest, can raise the `go`
  directive past your target.
- `marwan-at-work/mod`, `oligot/go-mod-upgrade` — focus on upgrades, not
  pinning to a Go version.
- See [proposal #65614](https://github.com/golang/go/issues/65614) — not
  implemented in the official toolchain.

## License

MIT
